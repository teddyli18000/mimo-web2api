// 凭据规范化：从 DevTools 复制时很容易带外层引号（"tok=="）。
// 这个 P0 踩过 —— 面板看着是绿的，但其实一条消息都发不出去。
// 断言口径是「上游实际收到了什么」，不是网关返回了什么。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest, dummyAccount } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

export default defineSuite({
  name: 'e2e/credentials',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack({
      accounts: [dummyAccount({ service_token: '"tok=="', ph: '"ph=="', user_id: '"1234567890"' })],
    });
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('带引号的凭据在 cookie 里只保留一层引号', async () => {
        upstream.reset();
        upstream.onChat({ text: 'ok' });
        const res = await api.chat(chatRequest());
        assert.equal(res.status, 200, res.text);

        const cookie = upstream.chatRequests().at(-1).headers.cookie;
        assert.match(cookie, /userId=1234567890/, cookie);
        assert.ok(!cookie.includes('""'), `cookie 里出现了双层引号: ${cookie}`);
        assert.match(cookie, /serviceToken=tok==/, cookie);
      });

      await t.test('ph 进 query 前已去引号', async () => {
        const req = upstream.chatRequests().at(-1);
        assert.equal(req.query.xiaomichatbot_ph, 'ph==');
      });

      await t.test('保存会话的 query 同样干净', async () => {
        await upstream.waitFor(
          () => upstream.saveRequests().length > 0,
          { timeout: 5000, message: '没有等到 conversation/save' },
        );
        const save = upstream.saveRequests().at(-1);
        assert.equal(save.query.xiaomichatbot_ph, 'ph==');
      });
    } finally {
      await stack.stop();
    }
  },
});
