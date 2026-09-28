// Parsing for the review output. The harness (opencode headless, --format
// json) streams JSONL events; the assistant's final answer is the joined text
// parts, which must contain the review JSON object.

// extractJsonObject returns the LAST brace-balanced `{...}` substring that
// contains a "verdict" key — the safety net when the model wraps its JSON in
// prose. It tries EVERY `{` (not just the first) so a prose brace like
// "{1: ...}" before the real JSON does not hide it, and it keeps the LAST
// match because earlier output is attacker-quotable: a PR can plant a fake
// `{"verdict": ...}` for the reviewer to quote, so the first match must not
// win — the model's final answer is always the last thing it emits.
// String/escape aware so braces inside strings don't break the scan.
export function extractJsonObject(s) {
  let best = '';
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
          if (cand.includes('"verdict"')) best = cand;
          break; // this start did not yield a deeper object; try the next
        }
      }
    }
  }
  return best;
}

// parseReview turns raw model output into the review object. Handles plain
// JSON, fenced JSON, and JSON embedded in prose; throws when no verdict
// object is present so the caller can retry.
export function parseReview(raw) {
  const cleaned = raw.trim().replace(/^```json\s*/i, '').replace(/^```\s*/i, '').replace(/```\s*$/i, '');
  let parsed = null;
  try {
    parsed = JSON.parse(cleaned);
  } catch {
    const cand = extractJsonObject(cleaned);
    if (cand) {
      try {
        parsed = JSON.parse(cand);
      } catch {
        parsed = null;
      }
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
