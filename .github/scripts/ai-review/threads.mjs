// Pure helpers for reading the PR author's replies to the bot's prior inline
// review comments and mapping a finding back to its thread root.

export function truncate(s, n) {
  s = (s || '').trim();
  return s.length > n ? `${s.slice(0, n)}…[truncated]` : s;
}

const MAX_REPLY_CHARS = 2000;

// The PR author's replies to the bot's prior inline findings (review comments
// with `in_reply_to_id` set), rendered for the prompt. A dropped finding's
// thread is then resolved by the caller.
export function buildReviewReplies(comments, { bot, author } = {}) {
  const repliesByRoot = new Map();
  for (const c of comments) {
    if (!c.in_reply_to_id) continue;
    if (!author || c.user?.login !== author) continue; // only the PR author
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

// ---- open prior threads + classification + parent-comment rendering ----

export const SEVERITY_ICONS = { blocking: '🛑', important: '⚠️', suggestion: '💡' };
const SEVERITY_RANK = { blocking: 0, important: 1, suggestion: 2 };
// Same regex the posting path uses, so a comment is trackable iff it is
// matchable here (a body posted without the tag would be invisible to both).
const SEVERITY_TAG = /\*\*\[(blocking|important)\]\*\*/;

// The bot's own thread roots that carry a severity tag — the candidates for
// "still open" once the caller subtracts what the GraphQL state marks resolved.
export function botFindingRoots(comments, { bot } = {}) {
  return comments.filter(
    (c) => !c.in_reply_to_id && (!bot || c.user?.login === bot) && SEVERITY_TAG.test(c.body || ''),
  );
}

// The bot's still-open finding roots: severity-tagged roots the thread state
// does NOT prove resolved. Without state (GraphQL failed), every tagged root
// counts as open — the conservative side: nothing resolves, everything stays
// listed. This filter is also what keeps the "Resolved this round" list from
// repeating: a thread closed in an earlier round is proven resolved here and
// can never be resolved (or re-listed) again.
export function stillOpen(roots, threadState) {
  return roots.filter((c) => {
    const st = threadState?.get(c.id);
    return !st || !st.isResolved;
  });
}

// Still-open prior findings as plain objects: what the prompt lists and what
// classification matches against. title is the first body line minus the tag.
export function buildOpenThreads(openRoots) {
  return openRoots.flatMap((c) => {
    const m = (c.body || '').match(SEVERITY_TAG);
    if (!m) return [];
    const title = (c.body || '').slice(m.index + m[0].length).trim().split('\n')[0];
    return [{
      rootId: c.id,
      path: c.path,
      line: typeof c.line === 'number' ? c.line : null,
      originalLine: typeof c.original_line === 'number' ? c.original_line : null,
      severity: m[1],
      title,
    }];
  });
}

// The OPEN PRIOR THREADS prompt block. One line per thread so the model can
// match by file + problem without re-reading the whole comment.
export function openThreadsPrompt(threads) {
  if (!threads.length) return '(no open prior threads)';
  return threads
    .map((t) => `- \`${t.path}:${t.line ?? t.originalLine ?? '?'}\` — ${t.severity} — ${truncate(t.title, 200)}`)
    .join('\n');
}

// exact: same path and the finding's line hits either of the thread's lines
// (null finding line matches a file-level thread on the same path). Closest
// thread when prior=true and the line drifted: same path, min distance.
function threadDistance(finding, t) {
  if (finding.path !== t.path) return Infinity;
  if (finding.line === null) return 0; // file-level matches any same-path thread
  const candidates = [t.line, t.originalLine].filter((l) => l !== null);
  return candidates.length ? Math.min(...candidates.map((l) => Math.abs(l - finding.line))) : 0;
}

// Split the round's findings into fresh (new table rows / new inline comments)
// and reflagged (attach to an existing open thread, always replied in-thread).
// An exact location match wins regardless of the model's prior flag (the
// duplicate-thread bug this replaces); the prior flag catches line drift.
// Suggestion entries never own threads, so they are always fresh.
export function classifyFindings(inline, openThreads) {
  const fresh = [];
  const reflagged = [];
  for (const f of inline) {
    if (f.severity !== 'suggestion') {
      const exact = openThreads.find((t) => threadDistance(f, t) === 0 && (f.line === null || [t.line, t.originalLine].includes(f.line)));
      if (exact) {
        reflagged.push({ ...f, thread: exact });
        continue;
      }
      if (f.prior) {
        let best = null;
        for (const t of openThreads) {
          const d = threadDistance(f, t);
          if (d !== Infinity && (!best || d < best.d)) best = { t, d };
        }
        if (best) {
          reflagged.push({ ...f, thread: best.t });
          continue;
        }
      }
    }
    fresh.push(f);
  }
  return { fresh, reflagged };
}

function blobUrl(owner, repo, headSha, path, line) {
  const anchor = typeof line === 'number' ? `#L${line}` : '';
  return `https://github.com/${owner}/${repo}/blob/${headSha}/${path}${anchor}`;
}

const commentUrl = (owner, repo, prNumber, id) =>
  `https://github.com/${owner}/${repo}/pull/${prNumber}#discussion_r${id}`;

// The findings table for the parent comment — one row per finding, severity
// icons, locations linked to the blob at the reviewed head, discussion links
// to the thread (re-flagged: its root; new: the comment posted this round via
// discussionUrls). Suggestions have no thread: they link the file only.
export function renderFindingsTable(findings, { owner, repo, prNumber, headSha, discussionUrls = new Map() }) {
  const rows = [...findings].sort(
    (a, b) => (SEVERITY_RANK[a.severity] ?? 9) - (SEVERITY_RANK[b.severity] ?? 9) || a.path.localeCompare(b.path),
  );
  return rows.map((f, i) => {
    const loc = `\`${f.path}${f.line !== null ? `:${f.line}` : ''}\``;
    const locLink = `[${loc}](${blobUrl(owner, repo, headSha, f.path, f.line)})`;
    const title = truncate(f.comment.split('\n')[0], 120);
    const discussionId =
      f.thread?.rootId ?? discussionUrls.get(`${f.path}:${f.line}`);
    const more = discussionId
      ? ` — [discussion](${commentUrl(owner, repo, prNumber, discussionId)})`
      : ' — [file](' + blobUrl(owner, repo, headSha, f.path, f.line) + ')';
    return `| ${i + 1} | ${SEVERITY_ICONS[f.severity] || f.severity} ${f.severity} | ${locLink} | ${title}${more} |`;
  });
}

// One ✅ bullet per thread the round resolved (the model dropped the finding
// and the open thread was closed).
export function renderResolved(resolvedRoots, { owner, repo, prNumber }) {
  if (!resolvedRoots.length) return [];
  const lines = resolvedRoots.map((t) => {
    const loc = `\`${t.path}:${t.line ?? t.originalLine ?? '?'}\``;
    return `- ✅ ${loc} — ${truncate(t.title, 120)} — [thread](${commentUrl(owner, repo, prNumber, t.rootId)})`;
  });
  lines.unshift(`**Resolved this round: ${resolvedRoots.length}**`);
  return lines;
}
