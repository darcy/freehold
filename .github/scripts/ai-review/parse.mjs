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
// plain JSON, fenced JSON, and JSON embedded in prose. Among shape-valid
// candidates the LAST one wins: the prompt requires the answer to end with
// the single review object, and earlier candidates in the same message are
// its own narration — or quoted, PR-planted fake verdicts. (A model that
// restates a plant AFTER its review violates the prompt; that residual is
// the reviewer prompt's job to flag, not the parser's to guess.) Throws when
// no shape-valid verdict is present so the caller can retry.
export function parseReview(raw) {
  const cleaned = raw.trim().replace(/^```json\s*/i, '').replace(/^```\s*/i, '').replace(/```\s*$/i, '');
  const candidates = [];
  try {
    candidates.push(JSON.parse(cleaned));
  } catch {
    // not plain JSON — the brace-balanced scan below still applies
  }
  for (const cand of extractJsonObject(cleaned)) {
    try {
      candidates.push(JSON.parse(cand));
    } catch {
      // skip unparseable candidate
    }
  }
  const valid = candidates.filter(p => p && REVIEW_SHAPE(p));
  const parsed = valid[valid.length - 1];
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error(`review output missing verdict JSON (raw head: ${raw.slice(0, 80)} | tail: ${raw.slice(-80)})`);
  }
  return parsed;
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
