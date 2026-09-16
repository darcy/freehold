// Reassemble an OpenAI-compatible streaming (SSE) completion into the single
// `message` object the non-streaming path would have returned. Streaming keeps
// response headers arriving immediately, which is what avoids undici's 5-minute
// headers timeout on slow reasoning models (the call itself can take minutes).
// Falls back to plain JSON if the provider ignored `stream: true`.
export function reassembleStream(text) {
  const t = text.trimStart();
  if (t.startsWith('{')) return JSON.parse(t).choices?.[0]?.message || null;

  const msg = { content: '', reasoning_content: '', tool_calls: [] };
  for (const line of text.split('\n')) {
    if (!line.startsWith('data:')) continue;
    const payload = line.slice(5).trim();
    if (!payload || payload === '[DONE]') continue;
    let chunk;
    try {
      chunk = JSON.parse(payload);
    } catch {
      continue;
    }
    const delta = chunk.choices?.[0]?.delta;
    if (!delta) continue;
    if (typeof delta.content === 'string') msg.content += delta.content;
    if (typeof delta.reasoning_content === 'string') msg.reasoning_content += delta.reasoning_content;
    for (const tc of delta.tool_calls || []) {
      const i = tc.index ?? 0;
      msg.tool_calls[i] ??= { function: { arguments: '' } };
      if (tc.function?.arguments) msg.tool_calls[i].function.arguments += tc.function.arguments;
    }
  }
  if (!msg.content && !msg.reasoning_content && msg.tool_calls.length === 0) return null;
  return msg;
}
