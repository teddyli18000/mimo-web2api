// OpenAI Responses API（/v1/responses）。
// 第一版覆盖文本：input 支持字符串与消息数组，instructions 前置，流式按官方事件名。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';
import { parse } from '../../harness/sse.mjs';

export default defineSuite({
  name: 'e2e/responses',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('字符串 input：返回 output_text 与 usage', async () => {
        upstream.onChat({ text: 'hello responses' });
        const res = await api.post('/v1/responses', { model: 'mimo-v2.6-pro', input: 'hi' });
        assert.equal(res.status, 200, res.text);
        assert.equal(res.json.object, 'response');
        assert.equal(res.json.status, 'completed');
        assert.equal(res.json.output[0].content[0].type, 'output_text');
        assert.equal(res.json.output[0].content[0].text, 'hello responses');
        assert.equal(res.json.usage.total_tokens, 19);
      });

      await t.test('消息数组 + instructions 都进上游 query', async () => {
        upstream.reset();
        upstream.onChat({ text: 'ok' });
        const res = await api.post('/v1/responses', {
          model: 'mimo-v2.6-pro',
          instructions: 'be terse',
          input: [
            { role: 'user', content: [{ type: 'input_text', text: 'first question' }] },
            { role: 'assistant', content: [{ type: 'output_text', text: 'first answer' }] },
            { role: 'user', content: [{ type: 'input_text', text: 'second question' }] },
          ],
        });
        assert.equal(res.status, 200, res.text);
        const query = upstream.chatRequests().at(-1).body.query;
        assert.ok(query.includes('be terse'), query);
        assert.ok(query.includes('second question'), query);
      });

      await t.test('流式：官方事件名与 delta', async () => {
        upstream.onChat({ text: 'streamed response' });
        const res = await api.post('/v1/responses', { model: 'mimo-v2.6-pro', input: 'hi', stream: true });
        assert.equal(res.status, 200, res.text);

        const events = parse(res.text);
        const names = events.map(e => e.event);
        assert.ok(names.includes('response.created'), names.join(','));
        assert.ok(names.includes('response.output_text.delta'), names.join(','));
        assert.ok(names.includes('response.completed'), names.join(','));

        const delta = events
          .filter(e => e.event === 'response.output_text.delta')
          .map(e => JSON.parse(e.data).delta)
          .join('');
        assert.equal(delta, 'streamed response');
      });

      await t.test('空 input 返回 400', async () => {
        const res = await api.post('/v1/responses', { model: 'mimo-v2.6-pro', input: '' });
        assert.equal(res.status, 400, res.text);
      });
    } finally {
      await stack.stop();
    }
  },
});
