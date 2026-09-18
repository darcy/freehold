// Pure helpers for reading the author's replies to the bot's prior inline
// review comments and mapping a finding back to its thread root.

export function truncate(s, n) {
  s = (s || '').trim();
  return s.length > n ? `${s.slice(0, n)}…[truncated]` : s;
}

const MAX_REPLY_CHARS = 2000;

// Author replies to the bot's prior inline findings (review comments with
// `in_reply_to_id` set), rendered for the prompt. A dropped finding's thread is
// then resolved by the caller (fixedComments).
export function buildReviewReplies(comments, bot) {
  const repliesByRoot = new Map();
  for (const c of comments) {
    if (!c.in_reply_to_id) continue;
    if (bot && c.user?.login === bot) continue; // the bot's own replies
    if (!repliesByRoot.has(c.in_reply_to_id)) repliesByRoot.set(c.in_reply_to_id, []);
    repliesByRoot.get(c.in_reply_to_id).push(c);
  }
  const lines = [];
  for (const root of comments) {
    if (root.in_reply_to_id) continue;
    if (bot && root.user?.login !== bot) continue; // only the bot's threads
    const replies = repliesByRoot.get(root.id);
    if (!replies) continue;
    const loc = `${root.path}:${root.line ?? root.original_line ?? '?'}`;
    lines.push(`- \`${loc}\` — ${truncate((root.body || '').split('\n')[0], 300)}`);
    for (const r of replies) {
      lines.push(`  - @${r.user?.login || 'unknown'}: ${truncate(r.body, MAX_REPLY_CHARS)}`);
    }
  }
  return lines.length ? lines.join('\n') : '(no replies to prior review comments)';
}

// Map a finding's path:line back to the bot's open thread root, plus whether
// that thread has a non-bot reply. A re-flagged finding with a reply is posted
// in-thread (createReplyForReviewComment) instead of as a new comment.
export function buildThreadIndex(comments, bot) {
  const index = new Map(); // 'path:line' -> { rootId, hasAuthorReply }
  const keysByRoot = new Map();
  for (const c of comments) {
    if (c.in_reply_to_id) continue;
    if (bot && c.user?.login !== bot) continue;
    if (!/\[(blocking|important)\]/.test(c.body || '')) continue;
    const keys = [];
    for (const line of [c.line, c.original_line]) {
      if (typeof line !== 'number') continue;
      keys.push(`${c.path}:${line}`);
    }
    keysByRoot.set(c.id, keys);
    for (const k of keys) index.set(k, { rootId: c.id, hasAuthorReply: false });
  }
  for (const c of comments) {
    if (!c.in_reply_to_id) continue;
    if (bot && c.user?.login === bot) continue;
    const keys = keysByRoot.get(c.in_reply_to_id);
    if (!keys) continue;
    for (const k of keys) {
      const entry = index.get(k);
      if (entry) entry.hasAuthorReply = true;
    }
  }
  return index;
}
