import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildReviewReplies, buildThreadIndex } from './threads.mjs';

const BOT = 'bot-review';

test('renders author replies on the bot\'s threads, ignoring bot replies and other threads', () => {
  const comments = [
    { id: 1, path: 'a.mjs', line: 10, body: '**[blocking]** boom', user: { login: BOT } },
    { id: 2, in_reply_to_id: 1, body: 'fixed in abc123', user: { login: 'alice' } },
    { id: 3, in_reply_to_id: 1, body: 'thanks', user: { login: BOT } },
    { id: 4, path: 'b.mjs', line: 5, body: '**[important]** other', user: { login: 'alice' } },
  ];
  const out = buildReviewReplies(comments, BOT);
  assert.match(out, /a\.mjs:10/);
  assert.match(out, /@alice: fixed in abc123/);
  assert.doesNotMatch(out, /thanks/);
  assert.doesNotMatch(out, /b\.mjs/);
});

test('thread index keys line and original_line and flags only non-bot replies', () => {
  const comments = [
    { id: 1, path: 'a.mjs', line: 10, original_line: 8, body: '**[blocking]** x', user: { login: BOT } },
    { id: 2, in_reply_to_id: 1, body: 'nope', user: { login: BOT } },
    { id: 3, path: 'c.mjs', line: null, original_line: 20, body: '**[important]** y', user: { login: BOT } },
    { id: 4, in_reply_to_id: 3, body: 'done', user: { login: 'alice' } },
    { id: 5, path: 'd.mjs', line: 1, body: 'no severity marker', user: { login: BOT } },
  ];
  const idx = buildThreadIndex(comments, BOT);
  assert.deepEqual(idx.get('a.mjs:10'), { rootId: 1, hasAuthorReply: false });
  assert.deepEqual(idx.get('a.mjs:8'), { rootId: 1, hasAuthorReply: false });
  assert.deepEqual(idx.get('c.mjs:20'), { rootId: 3, hasAuthorReply: true });
  assert.equal(idx.has('d.mjs:1'), false);
});

test('empty replies renders a stable placeholder', () => {
  assert.equal(buildReviewReplies([], BOT), '(no replies to prior review comments)');
});
