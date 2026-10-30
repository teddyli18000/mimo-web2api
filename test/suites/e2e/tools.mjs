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
