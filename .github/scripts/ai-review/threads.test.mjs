import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  buildReviewReplies,
  botFindingRoots,
  buildOpenThreads,
  stillOpen,
  openThreadsPrompt,
  classifyFindings,
  renderFindingsTable,
  renderResolved,
} from './threads.mjs';

const BOT = 'bot-review';
const AUTHOR = 'alice';

test('renders the author\'s replies on the bot\'s threads, ignoring bot and non-author replies', () => {
  const comments = [
    { id: 1, path: 'a.mjs', line: 10, body: '**[blocking]** boom', user: { login: BOT } },
    { id: 2, in_reply_to_id: 1, body: 'fixed in abc123', user: { login: AUTHOR } },
    { id: 3, in_reply_to_id: 1, body: 'thanks', user: { login: BOT } },
    { id: 4, path: 'b.mjs', line: 5, body: '**[important]** other', user: { login: AUTHOR } },
    { id: 5, path: 'c.mjs', line: 1, body: '**[important]** z', user: { login: BOT } },
    { id: 6, in_reply_to_id: 5, body: 'this is fine', user: { login: 'carol' } },
  ];
  const out = buildReviewReplies(comments, { bot: BOT, author: AUTHOR });
  assert.match(out, /a\.mjs:10/);
  assert.match(out, /@alice: fixed in abc123/);
  assert.doesNotMatch(out, /thanks/);
  assert.doesNotMatch(out, /b\.mjs/);
  assert.doesNotMatch(out, /c\.mjs/);
  assert.doesNotMatch(out, /carol/);
});

test('empty replies renders a stable placeholder', () => {
  assert.equal(buildReviewReplies([], { bot: BOT, author: AUTHOR }), '(no replies to prior review comments)');
});

const ROOTS = [
  { id: 1, path: 'a.mjs', line: 10, original_line: 8, body: '**[blocking]** boom\n- detail one\n- detail two', user: { login: BOT } },
  { id: 2, path: 'b.mjs', line: 20, original_line: null, body: '**[important]** leaky', user: { login: BOT } },
  { id: 3, path: 'c.mjs', line: 5, body: '**[suggestion]** table-only', user: { login: BOT } },
  { id: 4, path: 'd.mjs', line: 1, body: 'no severity tag', user: { login: BOT } },
  { id: 5, path: 'e.mjs', line: 3, body: '**[important]** not mine', user: { login: 'carol' } },
];

test('botFindingRoots: only the bot\'s severity-tagged thread roots', () => {
  const roots = botFindingRoots(ROOTS, { bot: BOT });
  assert.deepEqual(roots.map(r => r.id), [1, 2]);
});

test('buildOpenThreads: strips the tag, keeps both line anchors, tolerates null lines', () => {
  const [t1, t2] = buildOpenThreads(ROOTS);
  assert.deepEqual(t1, { rootId: 1, path: 'a.mjs', line: 10, originalLine: 8, severity: 'blocking', title: 'boom' });
  assert.equal(t2.line, 20);
  assert.equal(t2.originalLine, null);
  assert.equal(t2.title, 'leaky');
});

test('openThreadsPrompt: one line per thread, placeholder when empty', () => {
  const out = openThreadsPrompt(buildOpenThreads(ROOTS));
  assert.match(out, /`a\.mjs:10` — blocking — boom/);
  assert.match(out, /`b\.mjs:20` — important — leaky/);
  assert.equal(openThreadsPrompt([]), '(no open prior threads)');
});

test('classifyFindings: exact location wins regardless of the prior flag', () => {
  const { fresh, reflagged } = classifyFindings([
    { path: 'a.mjs', line: 10, severity: 'blocking', prior: false, comment: 'still boom' },
  ], buildOpenThreads(ROOTS));
  assert.equal(fresh.length, 0);
  assert.equal(reflagged.length, 1);
  assert.equal(reflagged[0].thread.rootId, 1);
});

test('classifyFindings: prior flag catches a shifted line (nearest same-file thread)', () => {
  const { fresh, reflagged } = classifyFindings([
    { path: 'a.mjs', line: 14, severity: 'blocking', prior: true, comment: 'moved down the file' },
  ], buildOpenThreads(ROOTS));
  assert.equal(reflagged.length, 1);
  assert.equal(reflagged[0].thread.rootId, 1);
  assert.equal(fresh.length, 0);
});

test('classifyFindings: prior with no open thread is fresh (fixed then regressed)', () => {
  const { fresh, reflagged } = classifyFindings([
    { path: 'zzz.mjs', line: 2, severity: 'blocking', prior: true, comment: 'came back' },
  ], buildOpenThreads(ROOTS));
  assert.equal(reflagged.length, 0);
  assert.equal(fresh.length, 1);
});

test('classifyFindings: suggestions never own threads, even at a tagged location', () => {
  const { fresh, reflagged } = classifyFindings([
    { path: 'a.mjs', line: 10, severity: 'suggestion', prior: false, comment: 'same line, softer take' },
  ], buildOpenThreads(ROOTS));
  assert.equal(reflagged.length, 0);
  assert.equal(fresh.length, 1);
});

const LINKS = { owner: 'o', repo: 'r', prNumber: 7, headSha: 'abc123' };

test('renderFindingsTable: severity-sorted rows, blob links, discussion beats file links', () => {
  const { fresh, reflagged } = classifyFindings([
    { path: 'x.mjs', line: 7, severity: 'important', prior: false, comment: 'new leak' },
    { path: 'a.mjs', line: 10, severity: 'blocking', prior: false, comment: 'still boom' },
    { path: 'c.mjs', line: 9, severity: 'suggestion', prior: false, comment: 'nice to have' },
  ], buildOpenThreads(ROOTS));
  const rows = renderFindingsTable([...reflagged, ...fresh], { ...LINKS, discussionUrls: new Map([['x.mjs:7', 999]]) });
  assert.match(rows[0], /^\| 1 \| 🛑 blocking \| \[`a\.mjs:10`\]\(https:\/\/github\.com\/o\/r\/blob\/abc123\/a\.mjs#L10\) \| still boom — \[discussion\]\(https:\/\/github\.com\/o\/r\/pull\/7#discussion_r1\)/);
  assert.match(rows[1], /^\| 2 \| ⚠️ important \| \[`x\.mjs:7`\].*— \[discussion\]\(.*#discussion_r999\)/);
  assert.match(rows[2], /^\| 3 \| 💡 suggestion \| \[`c\.mjs:9`\].*— \[file\]\(https:\/\/github\.com\/o\/r\/blob\/abc123\/c\.mjs#L9\)/);
});

test('renderFindingsTable: truncates long first lines to one row', () => {
  const rows = renderFindingsTable([
    { path: 'a.mjs', line: 1, severity: 'important', comment: `${'x'.repeat(200)}\nsecond line never shows` },
  ], LINKS);
  assert.equal(rows.length, 1);
  assert.doesNotMatch(rows[0], /second line/);
  assert.match(rows[0], /…\[truncated\]/);
});

test('renderResolved: ✅ bullets under a count header, empty when nothing resolved', () => {
  const out = renderResolved(buildOpenThreads(ROOTS).slice(0, 1), LINKS);
  assert.equal(out[0], '**Resolved this round: 1**');
  assert.match(out[1], /^- ✅ `a\.mjs:10` — boom — \[thread\]\(https:\/\/github\.com\/o\/r\/pull\/7#discussion_r1\)/);
  assert.deepEqual(renderResolved([], LINKS), []);
});

test('stillOpen: drops roots proven resolved, keeps the rest, keeps everything without state', () => {
  const roots = [
    { id: 1, path: 'a.mjs', line: 1, body: '**[important]** x', user: { login: BOT } },
    { id: 2, path: 'b.mjs', line: 2, body: '**[important]** y', user: { login: BOT } },
    { id: 3, path: 'c.mjs', line: 3, body: '**[important]** z', user: { login: BOT } },
  ];
  const state = new Map([
    [1, { threadId: 'T1', isResolved: true }],
    [2, { threadId: 'T2', isResolved: false }],
  ]);
  assert.deepEqual(stillOpen(roots, state).map(r => r.id), [2, 3]);
  assert.deepEqual(stillOpen(roots, null).map(r => r.id), [1, 2, 3]);
});
