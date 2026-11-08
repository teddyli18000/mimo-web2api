// OpenAI Responses API（/v1/responses）：文本、工具调用与完整事件序列。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';
import { parse } from '../../harness/sse.mjs';

const TOOLS = [{
  type: 'function',
  name: 'WebFetch',
  description: 'fetch a url',
  parameters: { type: 'object', properties: { url: { type: 'string' } }, required: ['url'] },
}];

const TOOL_TEXT = [
  '<tool_calls>',
  '<invoke name="WebFetch">',
  '<parameter name="url">https://example.com/data</parameter>',
  '</invoke>',
  '</tool_calls>',
].join('\n');

export default defineSuite({
  name: 'e2e/responses',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('字符串 input：返回 output_text 与 usage', async () => {
        upstream.onChat({ text: 'hello responses' });
        const res = await api.post('/v1/responses', { model: 'mimo-v2.6-pro', input: 'hi' });
        assert.equal(res.status, 200, res.text);
        assert.equal(res.json.object, 'response');
        assert.equal(res.json.status, 'completed');
        assert.equal(res.json.output[0].content[0].type, 'output_text');
        assert.equal(res.json.output[0].content[0].text, 'hello responses');
        assert.equal(res.json.usage.total_tokens, 19);
      });

      await t.test('消息数组 + instructions 都进上游 query', async () => {
        upstream.reset();
        upstream.onChat({ text: 'ok' });
        const res = await api.post('/v1/responses', {
          model: 'mimo-v2.6-pro',
          instructions: 'be terse',
          input: [
            { role: 'user', content: [{ type: 'input_text', text: 'first question' }] },
            { role: 'assistant', content: [{ type: 'output_text', text: 'first answer' }] },
            { role: 'user', content: [{ type: 'input_text', text: 'second question' }] },
          ],
        });
        assert.equal(res.status, 200, res.text);
        const query = upstream.chatRequests().at(-1).body.query;
        assert.ok(query.includes('be terse'), query);
        assert.ok(query.includes('second question'), query);
      });

      await t.test('流式：官方事件名与 delta', async () => {
        upstream.onChat({ text: 'streamed response' });
        const res = await api.post('/v1/responses', { model: 'mimo-v2.6-pro', input: 'hi', stream: true });
        assert.equal(res.status, 200, res.text);

        const events = parse(res.text);
        const names = events.map(e => e.event);
        assert.ok(names.includes('response.created'), names.join(','));
        assert.ok(names.includes('response.output_text.delta'), names.join(','));
        assert.ok(names.includes('response.completed'), names.join(','));

        const delta = events
          .filter(e => e.event === 'response.output_text.delta')
          .map(e => JSON.parse(e.data).delta)
          .join('');
        assert.equal(delta, 'streamed response');
      });

      // Responses 的工具调用走 function_call 项，不是 chat 的 tool_calls。
      await t.test('带 tools：输出 function_call 项（含 call_id / arguments）', async () => {
        upstream.reset();
        upstream.onChat({ text: TOOL_TEXT });
        const res = await api.post('/v1/responses', {
          model: 'mimo-v2.6-pro', input: 'fetch example.com', tools: TOOLS,
        });
        assert.equal(res.status, 200, res.text);

        const call = (res.json.output || []).find(o => o.type === 'function_call');
        assert.ok(call, `没有 function_call 项: ${JSON.stringify(res.json.output)}`);
        assert.equal(call.name, 'WebFetch');
        assert.equal(call.status, 'completed');
        assert.ok(call.call_id, '缺 call_id，客户端无法回传 function_call_output');
        assert.ok(call.id, '缺 item id');
        assert.deepEqual(JSON.parse(call.arguments), { url: 'https://example.com/data' });

        // 工具契约必须真的进了上游 query
        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(q.includes('<tool_calls>'), q.slice(-800));
        assert.ok(q.includes('WebFetch(url*:string)'), q.slice(-800));
      });

      await t.test('回传 function_call_output：工具结果进上游 query', async () => {
        upstream.reset();
        upstream.onChat({ text: TOOL_TEXT });
        const first = await api.post('/v1/responses', {
          model: 'mimo-v2.6-pro', input: 'fetch example.com', tools: TOOLS,
        });
        const call = first.json.output.find(o => o.type === 'function_call');

        upstream.onChat({ text: '页面内容是 hello' });
        const second = await api.post('/v1/responses', {
          model: 'mimo-v2.6-pro', tools: TOOLS,
          input: [
            { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'fetch example.com' }] },
            { type: 'function_call', call_id: call.call_id, name: call.name, arguments: call.arguments, id: call.id },
            { type: 'function_call_output', call_id: call.call_id, output: 'hello' },
          ],
        });
        assert.equal(second.status, 200, second.text);

        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(q.includes('[Tool Result]'), `工具结果没回传:\n${q}`);
        assert.ok(q.includes('hello'), q);
        assert.ok(!q.includes('fetch example.com'), `续轮不该重发历史:\n${q}`);
        assert.ok(q.includes('WebFetch(url*:string)'), '续轮丢了工具契约');
      });

      await t.test('流式 tools：官方事件序列与同一 response id', async () => {
        upstream.reset();
        upstream.onChat({ text: TOOL_TEXT });
        const res = await api.post('/v1/responses', {
          model: 'mimo-v2.6-pro', input: 'fetch example.com', tools: TOOLS, stream: true,
        });
        assert.equal(res.status, 200, res.text.slice(0, 400));

        const events = parse(res.text).map(e => {
          let data = null;
          try { data = JSON.parse(e.data); } catch { /* ignore */ }
          return { event: e.event, data };
        });
        const names = events.map(e => e.event);
        for (const want of [
          'response.created',
          'response.output_item.added',
          'response.function_call_arguments.delta',
          'response.function_call_arguments.done',
          'response.output_item.done',
          'response.completed',
        ]) {
          assert.ok(names.includes(want), `缺事件 ${want}: ${names.join(',')}`);
        }

        const created = events.find(e => e.event === 'response.created');
        const completed = events.find(e => e.event === 'response.completed');
        assert.equal(
          completed.data.response.id, created.data.response.id,
          'created 与 completed 的 response id 必须一致',
        );

        const added = events.find(e => e.event === 'response.output_item.added' && e.data.item?.type === 'function_call');
        assert.ok(added, '没有 function_call 的 output_item.added');
        assert.equal(added.data.item.name, 'WebFetch');

        const args = events
          .filter(e => e.event === 'response.function_call_arguments.delta')
          .map(e => e.data.delta).join('');
        assert.deepEqual(JSON.parse(args), { url: 'https://example.com/data' });

        const done = events.find(e => e.event === 'response.function_call_arguments.done');
        assert.equal(done.data.item_id, added.data.item.id, 'item_id 必须配对');

        const finalCall = (completed.data.response.output || []).find(o => o.type === 'function_call');
        assert.ok(finalCall, 'response.completed 里缺 function_call 项');
        assert.equal(finalCall.call_id, added.data.item.call_id);
      });

      await t.test('空 input 返回 400', async () => {
        const res = await api.post('/v1/responses', { model: 'mimo-v2.6-pro', input: '' });
        assert.equal(res.status, 400, res.text);
      });
    } finally {
      await stack.stop();
    }
  },
});
