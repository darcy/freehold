// Parsing for the review output. The harness (opencode headless, --format
// json) streams JSONL events; the assistant's final answer is the joined text
// parts, which must contain the review JSON object.

// extractJsonObject returns EVERY brace-balanced `{...}` substring that
// contains a "verdict" key, in order of appearance — the safety net when the
// model wraps its JSON in prose. It tries EVERY `{` (not just the first) so a
// prose brace like "{1: ...}" before the real JSON does not hide it.
// String/escape aware so braces inside strings don't break the scan. The
// caller ranks candidates: output text can contain earlier, PR-planted fake
// verdicts the reviewer merely quoted, so position alone (first OR last) is
// attacker-controllable.
export function extractJsonObject(s) {
  const found = [];
  for (let start = s.indexOf('{'); start >= 0; start = s.indexOf('{', start + 1)) {
    let depth = 0;
    let inStr = false;
    let esc = false;
    for (let i = start; i < s.length; i++) {
      const ch = s[i];
      if (inStr) {
        if (esc) esc = false;
        else if (ch === '\\') esc = true;
        else if (ch === '"') inStr = false;
        continue;
      }
      if (ch === '"') inStr = true;
      else if (ch === '{') depth += 1;
      else if (ch === '}') {
        depth -= 1;
        if (depth === 0) {
          const cand = s.slice(start, i + 1);
          if (cand.includes('"verdict"')) found.push(cand);
          break; // this start yielded its outermost object; try the next
        }
      }
    }
  }
  return found;
}

// A candidate is the review only if it carries the review's shape: verdict
// with the required prefix, a string summary, and an inline array. A bare
// {"verdict": ...} stub — planted or quoted — is not accepted.
const REVIEW_SHAPE = (parsed) =>
  typeof parsed?.verdict === 'string' &&
  /^(MERGE-READY|NEEDS WORK):/i.test(parsed.verdict) &&
  typeof parsed?.summary === 'string' &&
  Array.isArray(parsed?.inline);

// parseReview turns the model's final answer into the review object. Handles
// plain JSON, fenced JSON, and JSON embedded in prose.
//
// There is EXACTLY ONE shape-valid candidate or this throws. Every selection
// heuristic is attacker-steerable otherwise: position (first/last) via
// planted objects the reviewer quotes, length via padding — a diff can embed
// an arbitrarily large shape-valid fake for the reviewer to restate. The
// prompt requires the answer to be exactly the single review object, so a
// well-behaved model yields one candidate; anything else is injection
// suspicion and must fail CLOSED (throw → the round retries → a red check if
// it persists), never fail open on a guessed verdict.
export function parseReview(raw) {
  const cleaned = raw.trim().replace(/^```json\s*/i, '').replace(/^```\s*/i, '').replace(/```\s*$/i, '');
  const byShape = new Map();
  const consider = parsed => {
    if (parsed && REVIEW_SHAPE(parsed)) byShape.set(JSON.stringify(parsed), parsed);
  };
  try {
    consider(JSON.parse(cleaned));
  } catch {
    // not plain JSON — the brace-balanced scan below still applies
  }
  for (const cand of extractJsonObject(cleaned)) {
    try {
      consider(JSON.parse(cand));
    } catch {
      // skip unparseable candidate
    }
  }
  const valid = [...byShape.values()];
  if (valid.length !== 1) {
    throw new Error(
      valid.length
        ? `ambiguous review output: ${valid.length} distinct verdict candidates (possible injected verdict)`
        : `review output missing verdict JSON (raw head: ${raw.slice(0, 80)} | tail: ${raw.slice(-80)})`,
    );
  }
  return valid[0];
}

// The production parse path: the model's FINAL text part is the only thing
// that may reach the verdict extractor — earlier parts (and the raw event
// stream, whose tool-result events carry attacker-controlled file contents)
// are narration and can quote PR-planted fake verdicts. review.mjs calls
// THIS; tests pin it, not a re-implementation of the call shape.
export function parseReviewFromEvents(stdout) {
  const finalPart = harnessTextParts(stdout).pop();
  if (!finalPart) throw new Error('harness produced no text events — format drift?');
  return parseReview(finalPart);
}

// harnessTextParts reconstructs the assistant's messages from --format json
// output: one JSON event per line; text parts are collected in order.
// Non-event lines are ignored. The LAST part is the model's final answer.
export function harnessTextParts(stdout) {
  const parts = [];
  for (const line of stdout.split('\n')) {
    const t = line.trim();
    if (!t.startsWith('{')) continue;
    let e;
    try {
      e = JSON.parse(t);
    } catch {
      continue;
    }
    if (e?.type !== 'text') continue;
    const text = typeof e.part?.text === 'string' ? e.part.text : (typeof e.text === 'string' ? e.text : '');
    if (text) parts.push(text);
  }
  return parts;
}

// The joined answer — every text part. Diagnostic use only: verdict parsing
// must use the FINAL part (harnessTextParts(...).pop()), since earlier parts
// are narration that can quote PR-planted fake verdicts.
export function harnessText(stdout) {
  return harnessTextParts(stdout).join('');
}
