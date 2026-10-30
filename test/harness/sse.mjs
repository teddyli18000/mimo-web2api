// SSE 编解码。
//
// 网关的 ParseWebSSE 按「空行分帧」派发事件，且循环结束时不会 flush 缓冲区：
// 最后一个事件若没有尾随空行，会被静默丢掉。这里是唯一构造上游响应的地方，
// 帧一律以 \n\n 结尾；需要模拟「缺尾空行 / CRLF」时用 raw 场景直接回放字节。
export function frame({ id, event, data }) {
  let out = '';
  if (id !== undefined) out += `id: ${id}\n`;
  if (event !== undefined) out += `event: ${event}\n`;
  const payload = typeof data === 'string' ? data : JSON.stringify(data);
  for (const line of payload.split('\n')) out += `data: ${line}\n`;
  return out + '\n';
}

export function parse(text) {
  const events = [];
  let cur = { data: [] };
  const flush = () => {
    if (cur.id !== undefined || cur.event !== undefined || cur.data.length) {
      events.push({ id: cur.id, event: cur.event, data: cur.data.join('\n') });
    }
    cur = { data: [] };
  };
  for (const line of text.split(/\r?\n/)) {
    if (line === '') { flush(); continue; }
    if (line.startsWith('id:')) cur.id = line.slice(3).trim();
    else if (line.startsWith('event:')) cur.event = line.slice(6).trim();
    else if (line.startsWith('data:')) cur.data.push(line.slice(5).trim());
  }
  flush();
  return events;
}

// OpenAI 流式：data: {...} / data: [DONE]
export function parseOpenAIStream(text) {
  const chunks = [];
  for (const line of text.split(/\r?\n/)) {
    if (!line.startsWith('data:')) continue;
    const payload = line.slice(5).trim();
    if (payload === '[DONE]') { chunks.push({ done: true }); continue; }
    if (!payload) continue;
    try { chunks.push(JSON.parse(payload)); } catch { /* 注释帧忽略 */ }
  }
  return chunks;
}

// Anthropic 流式：event: xxx + data: {...}
export function parseAnthropicStream(text) {
  return parse(text).map(({ event, data }) => {
    let json = null;
    try { json = JSON.parse(data); } catch { /* 非 JSON */ }
    return { event, data: json ?? data };
  });
}

export function streamText(chunks) {
  let out = '';
  for (const c of chunks) {
    const delta = c?.choices?.[0]?.delta?.content;
    if (delta) out += delta;
  }
  return out;
}
