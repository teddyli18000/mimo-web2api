// OpenAI 兼容端点：真的网关进程 + 本地假上游，走完整 HTTP/SSE。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';
import { parseOpenAIStream } from '../../harness/sse.mjs';

export default defineSuite({
  name: 'e2e/openai',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream, gw } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('非流式：正文与 usage 透传', async () => {
        upstream.onChat({ text: 'hello from fake' });
        const res = await api.chat(chatRequest());
        assert.equal(res.status, 200, res.text);
        assert.equal(res.json.choices[0].message.content, 'hello from fake');
        assert.equal(res.json.usage.prompt_tokens, 12);
        assert.equal(res.json.usage.completion_tokens, 7);
        assert.equal(res.json.usage.total_tokens, 19);
      });

      await t.test('上游请求带上真实 cookie 与 ph', async () => {
        upstream.onChat({ text: 'ok' });
        await api.chat(chatRequest());
        const req = upstream.chatRequests().at(-1);
        assert.match(req.headers.cookie, /userId=1234567890/);
        assert.match(req.headers.cookie, /serviceToken=dummy-service-token/);
        assert.equal(req.query.xiaomichatbot_ph, 'dummy-ph==');
      });

      await t.test('流式：分帧合法、正文完整、以 [DONE] 收尾', async () => {
        upstream.onChat({ text: 'streamed answer' });
        const res = await api.chat(chatRequest({ stream: true }));
        assert.equal(res.status, 200, res.text);
        const chunks = parseOpenAIStream(res.text);
        assert.equal(chunks.at(-1).done, true, '最后一个必须是 [DONE]');

        const text = chunks
          .filter(c => c.choices)
          .map(c => c.choices[0]?.delta?.content || '')
          .join('');
        assert.equal(text, 'streamed answer');

        // 顺序与形状：正文 → finish_reason=stop → usage（choices 为空）→ [DONE]
        const finish = chunks.find(c => c.choices?.[0]?.finish_reason === 'stop');
        assert.ok(finish, `缺少 finish_reason=stop 的收尾块: ${res.text.slice(0, 600)}`);
        const usageChunk = chunks.find(c => c.usage);
        assert.ok(usageChunk, '缺少 usage 块');
        assert.equal(usageChunk.choices.length, 0, 'usage 块的 choices 必须是空数组');
        assert.ok(chunks.indexOf(finish) < chunks.indexOf(usageChunk), 'usage 块必须在收尾块之后');
      });

      await t.test('thinking 段不会漏进正文', async () => {
        upstream.onChat({ text: '<think>secret reasoning</think>visible' });
        const res = await api.chat(chatRequest());
        assert.equal(res.status, 200, res.text);
        const content = res.json.choices[0].message.content;
        assert.ok(!content.includes('secret'), content);
        assert.ok(content.includes('visible'), content);
      });

      await t.test('未知路径仍然回到 SPA，不误报 404', async () => {
        const res = await api.get('/some/spa/route');
        assert.equal(res.status, 200);
        assert.match(res.headers.get('content-type') || '', /text\/html/);
      });
    } finally {
      await stack.stop();
    }
  },
});
