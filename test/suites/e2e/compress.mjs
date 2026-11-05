// 上下文压缩：超长历史先让上游压成摘要，再拼最近一段原文。
// 断言两次上游调用：第一次是摘要请求，第二次带着 [历史摘要]。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

const LONG = 'important context line. '.repeat(9000); // 约 200KB，必然触发压缩

export default defineSuite({
  name: 'e2e/compress',
  tags: ['e2e'],
  timeout: 120_000,
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('超长历史先摘要再回答', async () => {
        upstream.reset();
        upstream.onChat({ text: 'SUMMARY-OF-HISTORY' }); // 摘要请求
        upstream.onChat({ text: 'final answer' });        // 正式请求

        const res = await api.chat(chatRequest({
          messages: [
            { role: 'system', content: 'you are a helpful agent' },
            { role: 'user', content: LONG },
            { role: 'assistant', content: 'noted the long context' },
            { role: 'user', content: 'step 2' },
            { role: 'assistant', content: 'done step 2' },
            { role: 'user', content: 'step 3' },
            { role: 'assistant', content: 'done step 3' },
            { role: 'user', content: 'step 4' },
            { role: 'assistant', content: 'done step 4' },
            { role: 'user', content: 'now summarize the task' },
          ],
        }), { timeout: 60_000 });

        assert.equal(res.status, 200, res.text);
        assert.equal(res.json.choices[0].message.content, 'final answer');

        const reqs = upstream.chatRequests();
        assert.equal(reqs.length, 2, `expected 2 upstream calls, got ${reqs.length}`);

        const [summaryReq, answerReq] = reqs;
        assert.ok(summaryReq.body.query.includes('压缩'), summaryReq.body.query.slice(0, 120));
        assert.ok(answerReq.body.query.includes('[历史摘要]'), answerReq.body.query.slice(0, 120));
        assert.ok(answerReq.body.query.includes('SUMMARY-OF-HISTORY'));
      }, { timeout: 90_000 });
    } finally {
      await stack.stop();
    }
  },
});
