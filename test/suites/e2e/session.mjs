// 会话复用：第二轮应复用 conversationId 且只发增量。
// 注意 parentId 目前不进请求体（web_client.go 里明确注释「当前协议不需要」），
// 所以断言口径是 conversationId + query，不要去断言 parentId 被传给上游。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

const REPLY = 'fake upstream reply';

export default defineSuite({
  name: 'e2e/session',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('第二轮复用会话，只发新增消息', async () => {
        upstream.reset();
        upstream.onChat({ text: REPLY });
        const first = await api.chat(chatRequest({ messages: [{ role: 'user', content: 'first question' }] }));
        assert.equal(first.status, 200, first.text);

        upstream.onChat({ text: REPLY });
        const second = await api.chat(chatRequest({
          messages: [
            { role: 'user', content: 'first question' },
            { role: 'assistant', content: REPLY },
            { role: 'user', content: 'second question' },
          ],
        }));
        assert.equal(second.status, 200, second.text);

        const [r1, r2] = upstream.chatRequests();
        assert.equal(r2.body.conversationId, r1.body.conversationId, '第二轮应复用同一个 conversationId');
        assert.ok(r2.body.query.includes('second question'), r2.body.query);
        assert.ok(!r2.body.query.includes('first question'), `第二轮不应重发完整历史: ${r2.body.query}`);
      });

      await t.test('客户端重放同一份历史会被当成新会话', async () => {
        upstream.reset();
        upstream.onChat({ text: REPLY });
        const payload = { messages: [{ role: 'user', content: 'replay question' }] };
        await api.chat(chatRequest(payload));
        upstream.onChat({ text: REPLY });
        await api.chat(chatRequest(payload));

        const [r1, r2] = upstream.chatRequests();
        assert.equal(r2.body.conversationId === r1.body.conversationId, false, '重放不应复用旧会话');
        assert.ok(r2.body.query.includes('replay question'), '新会话要发完整上下文');
      });
    } finally {
      await stack.stop();
    }
  },
});
