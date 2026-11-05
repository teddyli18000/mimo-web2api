// 真流式：断言「第一段内容到达的时间」远早于「整段结束的时间」。
//
// 之前的实现是「收完全部上游事件再一次性写响应」，用普通 api.chat() 读文本
// 根本看不出区别——两端拼接结果一模一样。所以这里必须自己读流并打时间点。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest, messagesRequest, dummyAccount } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';
import { parseOpenAIStream, parseAnthropicStream } from '../../harness/sse.mjs';

// 逐块读取网关响应，记录每一块到达的相对时间。
async function timeline(stack, path, body) {
  const start = Date.now();
  const res = await fetch(stack.gw.base + path, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${stack.gw.apiKey}`,
    },
    body: JSON.stringify(body),
  });
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  const marks = [];
  let text = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    const chunk = decoder.decode(value, { stream: true });
    text += chunk;
    marks.push({ at: Date.now() - start, chunk });
  }
  return { status: res.status, text, marks, total: Date.now() - start };
}

const joined = chunks => chunks
  .filter(c => c.choices)
  .map(c => c.choices[0]?.delta?.content || '')
  .join('');

export default defineSuite({
  name: 'e2e/streaming',
  tags: ['e2e'],
  async run(t) {
    // 两个账号：第一个被 401 摘除后，第二个必须顶上。
    const stack = await startStack({
      accounts: [
        dummyAccount(),
        dummyAccount({ id: 'acc-2', user_id: '2222222222', service_token: 'dummy-service-token-2' }),
      ],
    });
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('账号 401 被摘除，另一个账号继续服务', async () => {
        upstream.reset();
        upstream.onChat({ status: 401, body: { code: 401, msg: 'login required' } });
        upstream.onChat({ text: 'still alive' });

        const first = await api.chat(chatRequest());
        assert.equal(first.status, 502, first.text);

        const status = await api.get('/admin/api/accounts/status');
        assert.equal(status.status, 200, status.text);
        const down = status.json.accounts.filter(a => !a.healthy);
        assert.equal(down.length, 1, JSON.stringify(status.json.accounts));
        assert.ok(down[0].failures >= 1);
        assert.match(down[0].last_error, /401/, down[0].last_error);

        const second = await api.chat(chatRequest());
        assert.equal(second.status, 200, second.text);
        assert.equal(second.json.choices[0].message.content, 'still alive');
      });

      await t.test('OpenAI 流式：首段内容远早于整段结束', async () => {
        upstream.reset();
        upstream.onChat({
          chunks: [
            { event: 'dialogId', data: { content: 'dlg_slow' } },
            { event: 'message', data: { content: '第一段' }, delayMs: 400 },
            { event: 'message', data: { content: '第二段' }, delayMs: 400 },
            { event: 'message', data: { content: '第三段' }, delayMs: 400 },
            { event: 'usage', data: { promptTokens: 5, completionTokens: 6, totalTokens: 11 } },
            { event: 'finish', data: { content: '[DONE]' } },
          ],
        });

        const res = await timeline(stack, '/v1/chat/completions', chatRequest({ stream: true }));
        assert.equal(res.status, 200, res.text);

        const firstContent = res.marks.find(m => m.chunk.includes('第一段'));
        assert.ok(firstContent, `没收到第一段: ${res.text.slice(0, 400)}`);
        // 上游每段间隔 400ms：伪流式下首段要等 ~1.2s 才出现，真流式下 ~0.4s
        assert.ok(
          firstContent.at < res.total / 2,
          `首段到达太晚: first=${firstContent.at}ms total=${res.total}ms`,
        );
        assert.ok(!res.marks[0].chunk.includes('第三段'), '首块不应该已经包含全文');

        const chunks = parseOpenAIStream(res.text);
        assert.equal(joined(chunks), '第一段第二段第三段');
        assert.equal(chunks.at(-1).done, true);
      });

      await t.test('OpenAI 流式：工具标记跨 chunk 不泄漏', async () => {
        upstream.reset();
        const head = '我来查一下这个页面。';
        const tool = [
          '<function=WebFetch>',
          '<parameter name="url">https://example.com/data</parameter>',
          '</function>',
        ].join('\n');
        const whole = head + tool;
        // 故意切在标记中间：'<func' + 'tion=...'
        const cut = head.length + 5;
        upstream.onChat({
          chunks: [
            { event: 'dialogId', data: { content: 'dlg_tool' } },
            { event: 'message', data: { content: whole.slice(0, cut) }, delayMs: 30 },
            { event: 'message', data: { content: whole.slice(cut) }, delayMs: 30 },
            { event: 'usage', data: { promptTokens: 5, completionTokens: 6, totalTokens: 11 } },
            { event: 'finish', data: { content: '[DONE]' } },
          ],
        });

        const res = await api.chat(chatRequest({ stream: true, tools: [{ type: 'function', function: { name: 'WebFetch', parameters: { type: 'object' } } }] }));
        assert.equal(res.status, 200, res.text);
        const chunks = parseOpenAIStream(res.text);

        const body = joined(chunks);
        assert.ok(!body.includes('<function='), `工具标记漏进正文: ${body}`);
        assert.ok(!body.includes('<parameter'), `工具标记漏进正文: ${body}`);
        assert.ok(body.includes(head), `标记前的说明文字被吞了: ${body}`);

        const call = chunks.find(c => c.choices?.[0]?.delta?.tool_calls);
        assert.ok(call, `缺少 tool_calls 分片: ${res.text.slice(0, 600)}`);
        assert.equal(call.choices[0].delta.tool_calls[0].function.name, 'WebFetch');

        const args = chunks
          .flatMap(c => c.choices?.[0]?.delta?.tool_calls || [])
          .map(tc => tc.function?.arguments || '')
          .join('');
        assert.deepEqual(JSON.parse(args), { url: 'https://example.com/data' });
      });

      await t.test('Anthropic 流式：thinking 块先于正文，且逐段下发', async () => {
        upstream.reset();
        upstream.onChat({
          chunks: [
            { event: 'dialogId', data: { content: 'dlg_think' } },
            { event: 'message', data: { reasoningContent: '先想一下' }, delayMs: 200 },
            { event: 'message', data: { content: '答案是' }, delayMs: 200 },
            { event: 'message', data: { content: ' 42' }, delayMs: 600 },
            { event: 'usage', data: { promptTokens: 5, completionTokens: 6, totalTokens: 11 } },
            { event: 'finish', data: { content: '[DONE]' } },
          ],
        });

        const res = await timeline(stack, '/v1/messages', messagesRequest({
          stream: true,
          thinking: { type: 'enabled', budget_tokens: 12000 },
        }));
        assert.equal(res.status, 200, res.text);

        const events = parseAnthropicStream(res.text);
        const think = events.find(e => e.event === 'content_block_delta' && e.data?.delta?.type === 'thinking_delta');
        const text = events.find(e => e.event === 'content_block_delta' && e.data?.delta?.type === 'text_delta');
        assert.ok(think, `缺少 thinking_delta: ${res.text.slice(0, 400)}`);
        assert.ok(text, `缺少 text_delta: ${res.text.slice(0, 400)}`);
        assert.ok(events.indexOf(think) < events.indexOf(text), 'thinking 必须在正文之前');
        assert.equal(think.data.index, 0);
        assert.equal(text.data.index, 1);

        const names = events.map(e => e.event);
        assert.ok(names.includes('message_start'));
        assert.ok(names.includes('message_stop'));

        const textJoined = events
          .filter(e => e.event === 'content_block_delta' && e.data?.delta?.type === 'text_delta')
          .map(e => e.data.delta.text)
          .join('');
        assert.equal(textJoined, '答案是 42');

        const firstText = res.marks.find(m => m.chunk.includes('先想一下'));
        assert.ok(firstText, 'thinking 没有被提前下发');
        assert.ok(
          firstText.at < res.total / 2,
          `thinking 到达太晚: first=${firstText.at}ms total=${res.total}ms`,
        );
      });

      await t.test('已提交后的上游错误：走协议内 error 帧，不再回滚状态码', async () => {
        upstream.reset();
        upstream.onChat({
          chunks: [
            { event: 'dialogId', data: { content: 'dlg_err' } },
            { event: 'message', data: { content: '部分正文' }, delayMs: 30 },
            { event: 'error', data: { content: 'upstream exploded' } },
            { event: 'finish', data: { content: '[DONE]' } },
          ],
        });

        const res = await api.chat(chatRequest({ stream: true }));
        // 已经吐过正文：状态码停在 200，错误以 error 对象收尾
        assert.equal(res.status, 200, res.text);
        const chunks = parseOpenAIStream(res.text);
        const err = chunks.find(c => c.error);
        assert.ok(err, `缺少 error 帧: ${res.text.slice(0, 600)}`);
        assert.match(err.error.message, /upstream exploded/);
        assert.equal(chunks.at(-1).done, true, '错误后仍要以 [DONE] 收尾');
        assert.equal(joined(chunks), '部分正文');
      });
    } finally {
      await stack.stop();
    }
  },
});
