// Anthropic /v1/messages：Claude Code、Cline 走的就是这条。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, messagesRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';
import { parseAnthropicStream } from '../../harness/sse.mjs';

const textOf = content => (content || [])
  .filter(b => b.type === 'text')
  .map(b => b.text)
  .join('');

export default defineSuite({
  name: 'e2e/anthropic',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('非流式：文本块与 stop_reason', async () => {
        upstream.onChat({ text: 'anthropic reply' });
        const res = await api.messages(messagesRequest());
        assert.equal(res.status, 200, res.text);
        assert.equal(textOf(res.json.content), 'anthropic reply');
        assert.equal(res.json.stop_reason, 'end_turn');
        assert.ok(res.json.usage.input_tokens > 0);
      });

      await t.test('流式：事件序列完整', async () => {
        upstream.onChat({ text: 'streamed anthropic' });
        const res = await api.messages(messagesRequest({ stream: true }));
        assert.equal(res.status, 200, res.text);
        const events = parseAnthropicStream(res.text);
        const names = events.map(e => e.event);
        assert.ok(names.includes('message_start'), names.join(','));
        assert.ok(names.includes('content_block_delta'), names.join(','));
        assert.ok(names.includes('message_stop'), names.join(','));

        const text = events
          .filter(e => e.event === 'content_block_delta')
          .map(e => e.data?.delta?.text || '')
          .join('');
        assert.equal(text, 'streamed anthropic');
      });

      await t.test('system 数组真的进了上游 query', async () => {
        upstream.onChat({ text: 'ok' });
        const res = await api.messages(messagesRequest({ system: [{ type: 'text', text: 'be brief' }] }));
        assert.equal(res.status, 200, res.text);
        // 只断言 200 的话，把 system 丢掉也照样绿
        const query = upstream.chatRequests().at(-1).body.query;
        assert.ok(query.includes('be brief'), `system 内容没有进 query: ${query.slice(0, 200)}`);
      });
    } finally {
      await stack.stop();
    }
  },
});
