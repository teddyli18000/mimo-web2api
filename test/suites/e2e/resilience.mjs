// 失败路径：重试、上游 error 事件、空响应换会话。
// 时间常量来自代码：重试退避 2s/4s/8s/16s、空响应固定重试一次。
// 这里只用「两次 500 后成功」验证重试（约 6s）；「连续失败耗尽预算」要 46s+，不进 CI。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

export default defineSuite({
  name: 'e2e/resilience',
  tags: ['e2e'],
  timeout: 120_000,
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('上游 5xx：退避重试后成功', async () => {
        upstream.reset();
        upstream.onChat({ status: 500 });
        upstream.onChat({ status: 500 });
        upstream.onChat({ text: 'recovered' });

        const res = await api.chat(chatRequest());
        assert.equal(res.status, 200, res.text);
        assert.equal(res.json.choices[0].message.content, 'recovered');
        const reqs = upstream.chatRequests();
        assert.equal(reqs.length, 3, '应重试两次');
        // 上游按 msgId 去重：复用同一个 msgId 重发会被判「请求已处理」
        assert.notEqual(reqs[1].body.msgId, reqs[0].body.msgId, '重试必须重新生成 msgId');
        assert.equal(reqs[1].body.conversationId, reqs[0].body.conversationId, '同一会话内重试不应换 conversationId');
      }, { timeout: 40_000 });

      await t.test('上游 error 事件：即使已有正文也整体 502', async () => {
        upstream.reset();
        upstream.onChat({ text: 'partial answer', error: 'upstream exploded' });
        const res = await api.chat(chatRequest());
        assert.equal(res.status, 502, res.text);
        assert.ok(res.text.includes('upstream exploded'), res.text);
        assert.ok(!res.text.includes('partial answer'), '错误响应不应夹带半截正文');
      });
    } finally {
      await stack.stop();
    }

    // 空响应重试会改写会话映射，用一套干净的实例验，避免和上面的会话串味
    await t.test('空响应：换新会话重试，第三次沿用新会话', async () => {
      const s = await startStack();
      try {
        s.upstream.onChat({ empty: true });
        s.upstream.onChat({ text: 'recovered' });
        const first = await s.api.chat(chatRequest({ messages: [{ role: 'user', content: 'q1' }] }));
        assert.equal(first.status, 200, first.text);
        assert.equal(first.json.choices[0].message.content, 'recovered');

        const reqs = s.upstream.chatRequests();
        assert.equal(reqs.length, 2, '空响应应触发一次重试');
        assert.notEqual(reqs[1].body.conversationId, reqs[0].body.conversationId, '重试必须换新会话');

        s.upstream.onChat({ text: 'third' });
        const second = await s.api.chat(chatRequest({
          messages: [
            { role: 'user', content: 'q1' },
            { role: 'assistant', content: 'recovered' },
            { role: 'user', content: 'q2' },
          ],
        }));
        assert.equal(second.status, 200, second.text);

        const after = s.upstream.chatRequests();
        assert.equal(
          after.at(-1).body.conversationId,
          reqs[1].body.conversationId,
          '第三次应沿用重试后的新会话，而不是回到已失效的旧会话',
        );
      } finally {
        await s.stop();
      }
    }, { timeout: 60_000 });
  },
});
