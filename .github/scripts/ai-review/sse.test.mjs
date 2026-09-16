import { test } from 'node:test';
import assert from 'node:assert/strict';
import { reassembleStream } from './sse.mjs';

test('reassembles content + reasoning + tool-call argument fragments', () => {
  const stream = [
    'data: {"choices":[{"delta":{"reasoning_content":"think "}}]}',
    '',
    'data: {"choices":[{"delta":{"reasoning_content":"more"}}]}',
    '',
    'data: {"choices":[{"delta":{"content":"{\\"verdict\\""}}]}',
    '',
    'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\\"verdict\\":\\"MERGE"}}]}}]}',
    '',
    'data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"-READY\\"}"}}]}}]}',
    '',
    'data: [DONE]',
    '',
  ].join('\n');
  const msg = reassembleStream(stream);
  assert.equal(msg.reasoning_content, 'think more');
  assert.equal(msg.content, '{"verdict"');
  assert.equal(msg.tool_calls[0].function.arguments, '{"verdict":"MERGE-READY"}');
});

test('falls back to a plain (non-streaming) JSON body', () => {
  const msg = reassembleStream(JSON.stringify({ choices: [{ message: { content: 'hi' } }] }));
  assert.equal(msg.content, 'hi');
});

test('returns null for an empty stream', () => {
  assert.equal(reassembleStream('data: [DONE]\n'), null);
});
