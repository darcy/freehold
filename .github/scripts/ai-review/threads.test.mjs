import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildReviewReplies, buildThreadIndex } from './threads.mjs';

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

test('thread index keys line and original_line and flags only author replies', () => {
  const comments = [
    { id: 1, path: 'a.mjs', line: 10, original_line: 8, body: '**[blocking]** x', user: { login: BOT } },
    { id: 2, in_reply_to_id: 1, body: 'nope', user: { login: BOT } },
    { id: 3, path: 'c.mjs', line: null, original_line: 20, body: '**[important]** y', user: { login: BOT } },
    { id: 4, in_reply_to_id: 3, body: 'done', user: { login: AUTHOR } },
    { id: 5, path: 'd.mjs', line: 1, body: '**[important]** q', user: { login: BOT } },
    { id: 6, in_reply_to_id: 5, body: 'looks fine', user: { login: 'carol' } },
    { id: 7, path: 'e.mjs', line: 1, body: 'no severity marker', user: { login: BOT } },
  ];
  const idx = buildThreadIndex(comments, { bot: BOT, author: AUTHOR });
  assert.deepEqual(idx.get('a.mjs:10'), { rootId: 1, hasAuthorReply: false });
  assert.deepEqual(idx.get('a.mjs:8'), { rootId: 1, hasAuthorReply: false });
  assert.deepEqual(idx.get('c.mjs:20'), { rootId: 3, hasAuthorReply: true });
  assert.deepEqual(idx.get('d.mjs:1'), { rootId: 5, hasAuthorReply: false });
  assert.equal(idx.has('e.mjs:1'), false);
});

test('empty replies renders a stable placeholder', () => {
  assert.equal(buildReviewReplies([], { bot: BOT, author: AUTHOR }), '(no replies to prior review comments)');
});
