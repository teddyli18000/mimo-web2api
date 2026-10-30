// 通道选择：ultraspeed 的 chat 与「保存会话」都必须走 /fastchat/，
// 走错通道的典型症状是网页端历史丢失，而不是报错，所以必须断言上游收到的路径。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

const ULTRA = 'mimo-v2.6-pro-ultraspeed-studio';

export default defineSuite({
  name: 'e2e/channels',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('ultraspeed：chat 与 save 都走 fastchat', async () => {
        upstream.reset();
        upstream.onChat({ text: 'ultra reply' });
        const res = await api.chat(chatRequest({ model: ULTRA }));
        assert.equal(res.status, 200, res.text);

        const chatReq = upstream.chatRequests().at(-1);
        assert.equal(chatReq.path, '/fastchat/open-apis/bot/chat');
        await upstream.waitFor(
          () => upstream.saveRequests().some(r => r.path.startsWith('/fastchat/')),
          { timeout: 5000, message: 'ultraspeed 的会话保存没有走 fastchat 通道' },
        );
        // 只验路径不够：save 里写错 conversationId 的症状正是「网页端历史丢失」
        const save = upstream.saveRequests().find(r => r.path.startsWith('/fastchat/'));
        assert.equal(save.body.conversationId, chatReq.body.conversationId, 'save 的 conversationId 必须与 chat 一致');
      });

      await t.test('普通模型：chat 与 save 走 open-apis', async () => {
        upstream.reset();
        upstream.onChat({ text: 'normal reply' });
        const res = await api.chat(chatRequest({ model: 'mimo-v2.6-pro' }));
        assert.equal(res.status, 200, res.text);

        const chatReq = upstream.chatRequests().at(-1);
        assert.equal(chatReq.path, '/open-apis/bot/chat');
        await upstream.waitFor(
          () => upstream.saveRequests().some(r => r.path === '/open-apis/chat/conversation/save'),
          { timeout: 5000, message: '普通模型的会话保存没有走 open-apis 通道' },
        );
        const save = upstream.saveRequests().find(r => r.path === '/open-apis/chat/conversation/save');
        assert.equal(save.body.conversationId, chatReq.body.conversationId);
      });
    } finally {
      await stack.stop();
    }
  },
});
