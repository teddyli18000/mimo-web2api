// 工具调用：上游不会真的返回结构化 tool_calls，它只会吐一段约定格式的文本，
// 网关负责解析。这里直接喂那段文本，验证解析 + 转成 OpenAI / Anthropic 结构。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest, messagesRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';
import { parseOpenAIStream } from '../../harness/sse.mjs';

const TOOL_TEXT = [
  '<function=WebFetch>',
  '<parameter name="url">https://example.com/data</parameter>',
  '</function>',
].join('\n');

const TOOLS = [{
  type: 'function',
  function: {
    name: 'WebFetch',
    description: 'fetch a url',
    parameters: { type: 'object', properties: { url: { type: 'string' } }, required: ['url'] },
  },
}];

export default defineSuite({
  name: 'e2e/tools',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('OpenAI 非流式：文本被解析为 tool_calls', async () => {
        upstream.onChat({ text: TOOL_TEXT });
        const res = await api.chat(chatRequest({ tools: TOOLS, messages: [{ role: 'user', content: 'fetch example.com' }] }));
        assert.equal(res.status, 200, res.text);
        const message = res.json.choices[0].message;
        assert.ok(Array.isArray(message.tool_calls) && message.tool_calls.length === 1, JSON.stringify(message));
        assert.equal(message.tool_calls[0].function.name, 'WebFetch');
        const args = JSON.parse(message.tool_calls[0].function.arguments);
        assert.equal(args.url, 'https://example.com/data');
        assert.ok(!String(message.content || '').includes('<function='), '工具语法不应残留进正文');
      });

      await t.test('OpenAI 流式：tool_calls 走流式 chunk', async () => {
        upstream.onChat({ text: TOOL_TEXT });
        const res = await api.chat(chatRequest({
          stream: true,
          tools: TOOLS,
          messages: [{ role: 'user', content: 'fetch example.com' }],
        }));
        assert.equal(res.status, 200, res.text);
        const chunks = parseOpenAIStream(res.text);
        const withTools = chunks.find(c => c.choices?.[0]?.delta?.tool_calls);
        assert.ok(withTools, `没有 tool_calls chunk: ${res.text.slice(0, 400)}`);
        assert.equal(withTools.choices[0].delta.tool_calls[0].function.name, 'WebFetch');
      });

      // 上游网页端没有 tools 参数，模型只能照着 query 里的契约输出。
      // 契约丢了 = 模型按自己的习惯猜工具名（read_file / list_dir），
      // 客户端一个都不认，整轮工具调用全废。
      await t.test('上游 query 里带完整工具契约（格式 + 参数名）', async () => {
        upstream.reset();
        upstream.onChat({ text: 'ok' });
        await api.chat(chatRequest({ tools: TOOLS, messages: [{ role: 'user', content: 'hi' }] }));

        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(q.includes('<tool_calls>'), `缺少调用格式:\n${q.slice(-1200)}`);
        assert.ok(q.includes('<invoke name="'), `缺少调用格式:\n${q.slice(-1200)}`);
        assert.ok(q.includes('WebFetch(url*:string)'), `缺少工具签名:\n${q.slice(-1200)}`);
      });

      await t.test('续轮仍然带工具契约，不依赖上游上下文', async () => {
        upstream.reset();
        upstream.onChat({ text: 'first answer' });
        await api.chat(chatRequest({ tools: TOOLS, messages: [{ role: 'user', content: 'first question' }] }));

        upstream.onChat({ text: 'second answer' });
        await api.chat(chatRequest({
          tools: TOOLS,
          messages: [
            { role: 'user', content: 'first question' },
            { role: 'assistant', content: 'first answer' },
            { role: 'user', content: 'second question' },
          ],
        }));

        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(!q.includes('first question'), `续轮不该重发历史:\n${q}`);
        assert.ok(q.includes('second question'), q);
        assert.ok(q.includes('<tool_calls>'), '续轮丢了调用格式');
        assert.ok(q.includes('WebFetch(url*:string)'), '续轮丢了参数 schema');
      });

      // 50 个工具 + 4 万字符 system：旧实现从 sysStr 尾部截断，
      // 恰好把「怎么输出工具调用」这段切掉，模型于是乱猜。
      await t.test('system 极长时工具契约不会被截断', async () => {
        upstream.reset();
        upstream.onChat({ text: 'ok' });

        const many = Array.from({ length: 50 }, (_, i) => ({
          type: 'function',
          function: {
            name: `tool_${i}`,
            description: `tool number ${i} `.repeat(8),
            parameters: {
              type: 'object',
              properties: { [`p${i}`]: { type: 'string', description: `parameter ${i} `.repeat(8) } },
              required: [`p${i}`],
            },
          },
        }));

        const res = await api.chat(chatRequest({
          tools: many,
          messages: [
            { role: 'system', content: 'S'.repeat(100000) },
            { role: 'user', content: 'hi' },
          ],
        }));
        assert.equal(res.status, 200, res.text);

        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(q.includes('<tool_calls>'), `格式契约被截掉了:\n${q.slice(0, 500)}`);
        assert.ok(q.includes('<invoke name="'), '格式契约被截掉了');
        assert.ok(q.length <= 90000, `query 超预算: ${q.length}`);
      });

      await t.test('Anthropic：文本被解析为 tool_use 块', async () => {
        upstream.onChat({ text: TOOL_TEXT });
        const res = await api.messages(messagesRequest({
          max_tokens: 256,
          tools: [{ name: 'WebFetch', description: 'fetch a url', input_schema: { type: 'object', properties: { url: { type: 'string' } } } }],
          messages: [{ role: 'user', content: 'fetch example.com' }],
        }));
        assert.equal(res.status, 200, res.text);
        const block = (res.json.content || []).find(b => b.type === 'tool_use');
        assert.ok(block, JSON.stringify(res.json.content));
        assert.equal(block.name, 'WebFetch');
        assert.equal(block.input.url, 'https://example.com/data');
      });
    } finally {
      await stack.stop();
    }
  },
});
