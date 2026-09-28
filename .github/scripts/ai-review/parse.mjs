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
// with the required prefix and a string summary. A bare {"verdict": ...}
// stub — planted or quoted — is not accepted.
const REVIEW_SHAPE = (parsed) =>
  typeof parsed?.verdict === 'string' &&
  /^(MERGE-READY|NEEDS WORK):/i.test(parsed.verdict) &&
  typeof parsed?.summary === 'string';

// parseReview turns raw model output into the review object. Handles plain
// JSON, fenced JSON, and JSON embedded in prose; among the verdict-bearing
// candidates it takes the LONGEST shape-valid one — the real review carries
// the complete findings, planted or quoted stubs are short — and throws when
// no verdict object is present so the caller can retry.
export function parseReview(raw) {
  const cleaned = raw.trim().replace(/^```json\s*/i, '').replace(/^```\s*/i, '').replace(/```\s*$/i, '');
  let parsed = null;
  try {
    const direct = JSON.parse(cleaned);
    if (REVIEW_SHAPE(direct)) parsed = direct;
  } catch {
    // not plain JSON — fall through to candidate ranking
  }
  if (!parsed) {
    const candidates = extractJsonObject(cleaned)
      .map(cand => { try { return JSON.parse(cand); } catch { return null; } })
      .filter(p => p && REVIEW_SHAPE(p));
    if (candidates.length) {
      parsed = candidates.reduce((best, p) => (JSON.stringify(p).length > JSON.stringify(best).length ? p : best));
    }
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed) || !parsed.verdict) {
    throw new Error(`review output missing verdict JSON (raw head: ${raw.slice(0, 80)} | tail: ${raw.slice(-80)})`);
  }
  return parsed;
}

// harnessText reconstructs the assistant's answer from --format json output:
// one JSON event per line; the answer is the concatenation of the `text`
// events' part text. Non-event lines are ignored; returns '' when nothing
// parses (the caller then falls back to scanning the raw output).
export function harnessText(stdout) {
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
  return parts.join('');
}
