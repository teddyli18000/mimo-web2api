// 按真实客户端的连接方式打一遍，而不是按我们自己的路由表打。
//
// 触发这次用例的 bug：DSH 把 Base URL 配成 http://localhost:8080（不带 /v1），
// 自己去拼 /models、/chat/completions。网关只给 chat 注册了无前缀别名，
// /models 于是落到 SPA 兜底，返回 200 + index.html —— 客户端只报
// "http://localhost:8080/models did not answer with JSON"，
// 而所有 /v1/models 的用例都是绿的。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest, messagesRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';
import { parseOpenAIStream } from '../../harness/sse.mjs';

const TOOLS = [{
  type: 'function',
  function: {
    name: 'read',
    description: 'Path to read, resolved by the filesystem backend.',
    parameters: {
      type: 'object',
      properties: { file_path: { type: 'string', description: 'Path to read.' } },
      required: ['file_path'],
    },
  },
}];

const TOOL_CALL_TEXT = [
  '<tool_calls>',
  '<invoke name="read">',
  '<parameter name="file_path">C:\\a.txt</parameter>',
  '</invoke>',
  '</tool_calls>',
].join('\n');

const isJSON = res => String(res.headers.get('content-type') || '').includes('application/json');

export default defineSuite({
  name: 'e2e/client-compat',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('Base URL 不带 /v1：GET /models 返回 JSON 而不是 SPA 页面', async () => {
        const res = await api.get('/models');
        assert.equal(res.status, 200, res.text.slice(0, 300));
        assert.ok(isJSON(res), `content-type 不对: ${res.headers.get('content-type')}，body=${res.text.slice(0, 200)}`);
        assert.ok(!res.text.includes('<!doctype html>'), '返回了 SPA 的 index.html');
        const ids = (res.json?.data || []).map(m => m.id);
        assert.ok(ids.includes('mimo-fake-chat'), JSON.stringify(res.json).slice(0, 300));
      });

      await t.test('Base URL 不带 /v1：GET /models/{id} 返回单模型对象', async () => {
        const res = await api.get('/models/mimo-fake-chat');
        assert.equal(res.status, 200, res.text.slice(0, 300));
        assert.ok(isJSON(res), `content-type 不对: ${res.headers.get('content-type')}`);
        assert.equal(res.json.id, 'mimo-fake-chat', res.text.slice(0, 300));
      });

      await t.test('Base URL 不带 /v1：POST /chat/completions 正常出结果', async () => {
        upstream.reset();
        upstream.onChat({ text: 'no-prefix answer' });
        const res = await api.chat(chatRequest());
        assert.equal(res.status, 200, res.text.slice(0, 300));
        assert.ok(isJSON(res), `content-type 不对: ${res.headers.get('content-type')}`);
        assert.equal(res.json.choices[0].message.content, 'no-prefix answer');
      });

      await t.test('Base URL 不带 /v1：POST /responses 与 /messages 也可用', async () => {
        upstream.onChat({ text: 'responses answer' });
        const responses = await api.post('/responses', { model: 'mimo-v2.6-pro', input: 'hi' });
        assert.equal(responses.status, 200, responses.text.slice(0, 300));
        assert.ok(isJSON(responses), `content-type 不对: ${responses.headers.get('content-type')}`);

        upstream.onChat({ text: 'messages answer' });
        const messages = await api.post('/messages', messagesRequest());
        assert.equal(messages.status, 200, messages.text.slice(0, 300));
        assert.ok(isJSON(messages), `content-type 不对: ${messages.headers.get('content-type')}`);
      });

      await t.test('不存在的 API 路径返回 JSON 404，不能回落到 SPA', async () => {
        const res = await api.get('/v1/definitely-not-a-route');
        assert.equal(res.status, 404, res.text.slice(0, 300));
        assert.ok(isJSON(res), `content-type 不对: ${res.headers.get('content-type')}`);
        assert.ok(!res.text.includes('<!doctype html>'), '返回了 SPA 的 index.html');
        assert.equal(res.json?.error?.message, 'unknown endpoint');
      });

      // 完整 agent 循环：模型发起调用 → 客户端执行 → 结果回传 → 模型收尾。
      // 这条链路任何一环断掉，用户看到的都是"工具调用失败"。
      await t.test('工具循环：调用 → 结果回传 → 继续回答', async () => {
        upstream.reset();
        upstream.onChat({ text: TOOL_CALL_TEXT });

        const first = await api.chat(chatRequest({
          tools: TOOLS,
          messages: [{ role: 'user', content: '读一下 a.txt' }],
        }));
        assert.equal(first.status, 200, first.text.slice(0, 400));
        const call = first.json.choices[0].message.tool_calls?.[0];
        assert.ok(call, `没有解析出 tool_calls: ${first.text.slice(0, 400)}`);
        assert.equal(call.function.name, 'read');
        assert.equal(call.type, 'function');
        assert.ok(call.id, 'tool_call 缺 id，客户端无法回传结果');
        assert.deepEqual(JSON.parse(call.function.arguments), { file_path: 'C:\\a.txt' });

        upstream.onChat({ text: '文件里是 hello' });
        const second = await api.chat(chatRequest({
          tools: TOOLS,
          messages: [
            { role: 'user', content: '读一下 a.txt' },
            { role: 'assistant', content: null, tool_calls: [call] },
            { role: 'tool', tool_call_id: call.id, content: 'hello' },
          ],
        }));
        assert.equal(second.status, 200, second.text.slice(0, 400));
        assert.equal(second.json.choices[0].message.content, '文件里是 hello');

        const query = upstream.chatRequests().at(-1).body.query;
        assert.ok(query.includes('[Tool Result]'), `工具结果没有回传给上游:\n${query}`);
        assert.ok(query.includes('hello'), query);
        assert.ok(query.includes('read(file_path*:string)'), `续轮丢了工具契约:\n${query}`);
      });

      // 结构性防回归：/v1 下每一条路由都必须有等效的无前缀别名。
      // 漏注册一条的后果就是这次的事故——客户端拿到 SPA 页面而不是 JSON。
      await t.test('/v1 下每条 API 都有等效的无前缀别名', async () => {
        upstream.reset();
        upstream.setChatDefault({ text: 'alias ok' });

        const pairs = [
          { method: 'GET', path: '/models' },
          { method: 'GET', path: '/models/mimo-fake-chat' },
          { method: 'POST', path: '/chat/completions', body: chatRequest() },
          { method: 'POST', path: '/responses', body: { model: 'mimo-v2.6-pro', input: 'hi' } },
          { method: 'POST', path: '/messages', body: messagesRequest() },
          { method: 'POST', path: '/messages/count_tokens', body: { model: 'mimo-v2.6-pro', messages: [{ role: 'user', content: 'hi' }] } },
        ];

        for (const p of pairs) {
          const withPrefix = await api.request(`/v1${p.path}`, { method: p.method, body: p.body });
          const withoutPrefix = await api.request(p.path, { method: p.method, body: p.body });
          assert.notEqual(withPrefix.status, 404, `${p.path} 在 /v1 下都不存在`);
          assert.equal(
            withoutPrefix.status,
            withPrefix.status,
            `${p.method} ${p.path} 与 /v1 版本状态不一致（${withoutPrefix.status} vs ${withPrefix.status}）: ${withoutPrefix.text.slice(0, 200)}`,
          );
          assert.equal(
            isJSON(withoutPrefix),
            isJSON(withPrefix),
            `${p.method} ${p.path} 的 content-type 与 /v1 版本不一致`,
          );
        }
      });

      // 严格按 OpenAI 流式规范检查，任何一条不满足，增量解析型客户端都会崩。
      await t.test('流式工具调用的 SSE 契约', async () => {
        upstream.reset();
        upstream.onChat({ text: TOOL_CALL_TEXT });
        const res = await api.chat(chatRequest({
          stream: true,
          tools: TOOLS,
          messages: [{ role: 'user', content: '读一下 a.txt' }],
        }));

        const chunks = parseOpenAIStream(res.text);
        assert.equal(chunks.at(-1)?.done, true, `最后一个必须是 [DONE]: ${res.text.slice(-200)}`);

        const body = chunks.filter(c => !c.done);
        const ids = new Set(body.map(c => c.id));
        assert.equal(ids.size, 1, `同一个流必须共用 id: ${[...ids].join(',')}`);

        const first = body[0];
        assert.equal(first.choices?.[0]?.delta?.role, 'assistant', '首帧必须声明 assistant role');

        const fragments = body.flatMap(c => c.choices?.[0]?.delta?.tool_calls || []);
        assert.ok(fragments.length >= 2, '工具调用应拆成 name 与 arguments 两段');
        for (const f of fragments) {
          assert.equal(typeof f.index, 'number', `分片缺 index: ${JSON.stringify(f)}`);
        }
        assert.equal(fragments[0].function.name, 'read');
        const args = fragments.map(f => f.function?.arguments || '').join('');
        assert.deepEqual(JSON.parse(args), { file_path: 'C:\\a.txt' });

        const finishes = body.filter(c => c.choices?.[0]?.finish_reason != null);
        assert.equal(finishes.length, 1, `finish_reason 只能出现一次: ${JSON.stringify(finishes.map(f => f.choices[0].finish_reason))}`);
        assert.equal(finishes[0].choices[0].finish_reason, 'tool_calls');

        // usage 必须带 choices: []，否则客户端会当成又一条正文/工具分片
        const usage = body.find(c => c.usage);
        assert.ok(usage, '缺少 usage 块');
        assert.deepEqual(usage.choices, []);
      });
    } finally {
      await stack.stop();
    }
  },
});
