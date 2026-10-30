// SSE 解析的真实边界：直接回放字节，而不是用 sse.frame() 构造「理想帧」。
// 现有解析器有两个已知特性，这里把它们钉住：
// 1. 循环结束不 flush —— 最后一个事件没有尾随空行就会被丢掉；
// 2. 分帧只认空行，CRLF 能处理（依赖每行 TrimSpace）。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';
import { frame } from '../../harness/sse.mjs';

export default defineSuite({
  name: 'e2e/sse-edges',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('缺尾空行：最后一个事件被静默丢弃', async () => {
        const raw = frame({ id: 'm1', event: 'message', data: { type: 'text', content: 'hello' } })
          + 'event: usage\ndata: {"promptTokens":12,"completionTokens":7,"totalTokens":19}';
        upstream.onChat({ raw });

        const res = await api.chat(chatRequest());
        assert.equal(res.status, 200, res.text);
        assert.equal(res.json.choices[0].message.content, 'hello');
        assert.equal(
          res.json.usage,
          undefined,
          '缺少尾随空行时 usage 事件应被丢弃（解析器循环结束不 flush）；行为若变了要显式改这条断言',
        );
      });

      await t.test('CRLF 帧可以正常解析', async () => {
        const raw = 'id: m2\r\nevent: message\r\ndata: {"type":"text","content":"crlf ok"}\r\n\r\n';
        upstream.onChat({ raw });

        const res = await api.chat(chatRequest());
        assert.equal(res.status, 200, res.text);
        assert.equal(res.json.choices[0].message.content, 'crlf ok');
      });
    } finally {
      await stack.stop();
    }
  },
});
