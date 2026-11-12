// 提示词清洗：用假上游模拟「按连续短语判风控」，证明清洗层真的能让请求通过，
// 同时验证用户数据逐字不动、工具契约不受影响、会话续轮不断链。
//
// 参考 ardeyouxipianyi/workbuddy2api-hub 的 sanitize_messages 方法论（规则表、
// 按 role、最小改写、幂等、快路径），但规则集针对我们自己的上游与客户端。
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const SYSTEM = readFileSync(path.join(HERE, '../../compat/fixtures/dsh-system.txt'), 'utf8');

// 客户端注入的连续指纹短语；上游按整段命中
const FINGERPRINTS = [
  'You are an AI agent powered by DeepSeek Harness.',
  'are paths the user explicitly referenced.',
  'Web GUI whose URL the runtime context names.',
];

const QUESTION = '请读一下 README.md 的前 20 行，并告诉我第 3 行写了什么。';
const KEEP = [
  'mimo-v2.6-pro',                       // 模型身份信息不能丢
  'DeepSeek Harness',                    // 保守模式连品牌全名都保留
  'Tokens prefixed with @',              // 整条 @ 引用规则要保留（只补一个词）
  'Relative paths resolve from',
  'The runtime context names the DeepSeek Harness implementation checkout.',
];

async function withStack(env, fn) {
  const stack = await startStack({ gatewayOptions: { env } });
  try {
    await fn(stack);
  } finally {
    await stack.stop();
  }
}

export default defineSuite({
  name: 'e2e/prompt-clean',
  tags: ['e2e'],
  // 每条用例隔离一个网关进程（PROMPT_CLEAN 是进程级环境变量），
  // 8 个 stack 在慢 CI 机器上需要比默认 180s 更宽的余量。
  timeout: 300_000,
  async run(t) {
    // fail-closed 只做一次：每个用例都新建 stack，不必重复打 health
    await withStack({}, async ({ api, upstream }) => {
      await assertUpstreamIsLocal(t, { api, upstream });
    });

    // 记录清洗前后同一请求的 query，末尾比较长度变化
    const observed = { cleanQuery: null, offQuery: null };

    await t.test('默认清洗：上游按指纹判风控时请求能通过', async () => {
      await withStack({ PROMPT_CLEAN: 'conservative' }, async ({ api, upstream }) => {
        upstream.setRiskFilter(FINGERPRINTS);
        upstream.onChat({ text: 'README.md 第 3 行写的是「安装」一节。' });

        const res = await api.chat(chatRequest({
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: QUESTION }],
        }));
        assert.equal(res.status, 200, res.text.slice(0, 300));
        assert.equal(upstream.riskHits, 0, `风控不该命中，实际命中 ${upstream.riskHits} 次`);
        assert.equal(res.json.choices[0].message.content, 'README.md 第 3 行写的是「安装」一节。');

        const q = upstream.chatRequests().at(-1).body.query;
        observed.cleanQuery = q;
        for (const fp of FINGERPRINTS) {
          assert.ok(!q.includes(fp), `指纹没被清洗掉: ${fp}`);
        }
        // 零信息损失：关键内容逐字保留
        for (const keep of KEEP) {
          assert.ok(q.includes(keep), `清洗丢了关键信息: ${keep}`);
        }
        // 用户提问逐字不动
        assert.ok(q.includes(QUESTION), '用户提问被改写了');
      });
    });

    await t.test('关掉清洗（PROMPT_CLEAN=off）：同一请求应被风控拒绝', async () => {
      // 对照组：证明上一条的绿不是「风控模拟没生效」
      await withStack({ PROMPT_CLEAN: 'off' }, async ({ api, upstream }) => {
        upstream.setRiskFilter(FINGERPRINTS);
        upstream.onChat({ text: 'should not be reached' });

        const res = await api.chat(chatRequest({
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: QUESTION }],
        }));
        assert.notEqual(res.status, 200, '未清洗时应被上游风控拒绝');
        assert.ok(upstream.riskHits >= 1, '风控模拟应命中（网关可能重试 4xx）');
        const q = upstream.chatRequests().at(-1).body.query;
        observed.offQuery = q;
        assert.ok(q.includes(FINGERPRINTS[0]), 'off 模式下原文应原样送出');

        // 风控模拟必须真的会拒绝原文：直接把原文当 query 打一次上游，
        // 否则「默认清洗通过」可能只是风控模拟形同虚设。
        const probe = await fetch(`${upstream.base}/open-apis/react/chat`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ query: SYSTEM }),
        });
        assert.equal(probe.status, 400, '风控模拟没有拒绝含指纹的原文');
      });
    });

    await t.test('用户数据逐字不动：同句出现在 user 里不被改写', async () => {
      await withStack({}, async ({ api, upstream }) => {
        upstream.onChat({ text: 'ok' });
        const userText = 'Tokens prefixed with @ are paths the user explicitly referenced. 请解释这句话。';
        const res = await api.chat(chatRequest({
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: userText }],
        }));
        assert.equal(res.status, 200, res.text.slice(0, 300));
        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(q.includes(userText), 'user 消息被改写了');
      });
    });

    await t.test('工具契约不受清洗影响', async () => {
      await withStack({}, async ({ api, upstream }) => {
        upstream.onChat({ text: 'ok' });
        const tools = [{
          type: 'function',
          function: {
            name: 'read',
            description: 'Path to read, resolved by the filesystem backend.',
            parameters: {
              type: 'object',
              properties: { file_path: { type: 'string' } },
              required: ['file_path'],
            },
          },
        }];
        const res = await api.chat(chatRequest({
          tools,
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: QUESTION }],
        }));
        assert.equal(res.status, 200, res.text.slice(0, 300));
        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(q.includes('<tool_calls>'), '调用格式契约被清洗掉了');
        assert.ok(q.includes('read(file_path*:string)'), '工具签名被改写了');
        assert.ok(q.includes('Every parameter marked *'), '参数规则被改写了');
      });
    });

    await t.test('续轮会话不断链：清洗不改变 fingerprint', async () => {
      await withStack({}, async ({ api, upstream }) => {
        upstream.setChatDefault({ text: 'ok' });
        const first = chatRequest({
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: '第一问' }],
        });
        const a = await api.chat(first);
        assert.equal(a.status, 200, a.text.slice(0, 300));

        const second = await api.chat(chatRequest({
          messages: [
            { role: 'system', content: SYSTEM },
            { role: 'user', content: '第一问' },
            { role: 'assistant', content: 'ok' },
            { role: 'user', content: '第二问' },
          ],
        }));
        assert.equal(second.status, 200, second.text.slice(0, 300));
        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(q.includes('第二问'), q);
        assert.ok(!q.includes('第一问'), `续轮不该重发完整历史:\n${q.slice(-400)}`);
      });
    });

    await t.test('三条协议都清洗', async () => {
      await withStack({}, async ({ api, upstream }) => {
        upstream.setChatDefault({ text: 'ok' });
        const aTools = [{
          name: 'read', description: 'read a file',
          input_schema: { type: 'object', properties: { file_path: { type: 'string' } } },
        }];
        const rTools = [{
          type: 'function', name: 'read', description: 'read a file',
          parameters: { type: 'object', properties: { file_path: { type: 'string' } } },
        }];

        const chat = await api.chat(chatRequest({
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: 'hi' }],
        }));
        const anthropic = await api.messages({
          model: 'mimo-v2.6-pro', max_tokens: 128, tools: aTools,
          system: SYSTEM, messages: [{ role: 'user', content: 'hi' }],
        });
        const responses = await api.post('/v1/responses', {
          model: 'mimo-v2.6-pro', tools: rTools,
          instructions: SYSTEM, input: 'hi',
        });

        for (const [name, res] of [['chat', chat], ['anthropic', anthropic], ['responses', responses]]) {
          assert.equal(res.status, 200, `${name}: ${res.text.slice(0, 200)}`);
        }
        const chatReqs = upstream.chatRequests();
        const withSystem = chatReqs.filter(r => String(r.body?.query ?? '').includes('mimo-v2.6-pro'));
        // 防假绿：三条协议的 system 都必须真的进了 query，否则「没有指纹」只是空转
        assert.ok(withSystem.length >= 3,
          `只有 ${withSystem.length}/${chatReqs.length} 条请求带了 system: ` +
          chatReqs.map(r => String(r.body?.query ?? '').slice(0, 60)).join(' | '));
        for (const req of chatReqs) {
          for (const fp of FINGERPRINTS) {
            assert.ok(!String(req.body?.query ?? '').includes(fp), `协议请求里残留指纹: ${fp}`);
          }
        }
      });
    });

    // 真机实测：模型会吐一个没有任何 invoke 的空 <tool_calls> 块，
    // 解析出 0 个调用时误判补发会把内部标记原样发给客户端。
    //
    // 注意必须解析 SSE 里的 JSON 再取 content：Go 的 encoding/json 会把
    // `<` 转义成 `\u003c`，直接对原始响应文本做 includes('<tool_calls>')
    // 会永远为 false —— 那样用例就是假绿。
    await t.test('空工具块不漏进流式 content（三条协议）', async () => {
      // 取出所有 data: 帧里实际下发给客户端的文本（OpenAI / Responses / Anthropic 三种形状）
      const streamedText = raw => raw.split(/\r?\n/)
        .filter(l => l.startsWith('data: ') && !l.includes('[DONE]'))
        .map(l => { try { return JSON.parse(l.slice(6)); } catch { return null; } })
        .filter(Boolean)
        .map(o => {
          if (o.choices?.[0]?.delta?.content) return o.choices[0].delta.content;      // OpenAI
          if (o.delta?.text) return o.delta.text;                                     // Anthropic text_delta
          if (o.delta?.type === 'text_delta') return o.delta.text || '';
          if (o.type === 'response.output_text.delta') return o.delta || '';           // Responses
          return '';
        }).join('');

      await withStack({}, async ({ api, upstream }) => {
        const emptyBlock = '这是一段正常回答。\n<tool_calls>\n</tool_calls>';
        upstream.setChatDefault({ text: emptyBlock });

        const chat = await api.chat(chatRequest({
          stream: true,
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: 'hi' }],
        }));
        const chatText = streamedText(chat.text);
        assert.ok(chatText.includes('这是一段正常回答'), `正文被清洗掉了: ${JSON.stringify(chatText)}`);
        assert.ok(!chatText.includes('<tool_calls>'),
          `OpenAI 流式漏出工具标记: ${JSON.stringify(chatText)}`);

        const anthropic = await api.messages({
          model: 'mimo-v2.6-pro', max_tokens: 128, stream: true,
          system: SYSTEM, messages: [{ role: 'user', content: 'hi' }],
        });
        const anthropicText = streamedText(anthropic.text);
        assert.ok(!anthropicText.includes('<tool_calls>'),
          `Anthropic 流式漏出工具标记: ${JSON.stringify(anthropicText)}`);

        const responses = await api.post('/v1/responses', {
          model: 'mimo-v2.6-pro', stream: true, input: 'hi',
        });
        const responsesText = streamedText(responses.text);
        assert.ok(!responsesText.includes('<tool_calls>'),
          `Responses 流式漏出工具标记: ${JSON.stringify(responsesText)}`);
      });
    });

    // 区间判定：尾部纯工具语法整段截掉，正文一个字不动
    const contentOf = raw => raw.split(/\r?\n/)
      .filter(l => l.startsWith('data: ') && !l.includes('[DONE]'))
      .map(l => { try { return JSON.parse(l.slice(6)); } catch { return null; } })
      .filter(Boolean)
      .map(o => o.choices?.[0]?.delta?.content || '').join('');

    await t.test('正文 + 尾部残片：残片清掉、正文完整保留', async () => {
      await withStack({}, async ({ api, upstream }) => {
        upstream.setChatDefault({ text: '这是结论，请照抄。\n<tool_calls>\n</invoke>' });
        const res = await api.chat(chatRequest({
          stream: true,
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: 'hi' }],
        }));
        const text = contentOf(res.text);
        assert.equal(text.includes('这是结论，请照抄。'), true, `正文被删改: ${JSON.stringify(text)}`);
        assert.ok(!text.includes('</invoke>') && !text.includes('<tool_calls>'),
          `残片没清掉: ${JSON.stringify(text)}`);
      });
    });

    await t.test('正文引用裸标签：一个字都不能删', async () => {
      await withStack({}, async ({ api, upstream }) => {
        upstream.setChatDefault({ text: '请解释 <tool_calls> 这个标签的用法。' });
        const res = await api.chat(chatRequest({
          stream: true,
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: 'hi' }],
        }));
        const text = contentOf(res.text);
        assert.ok(text.includes('请解释 <tool_calls> 这个标签的用法。'),
          `正文里的裸标签被误删: ${JSON.stringify(text)}`);
      });
    });

    await t.test('清洗后的 query 长度变化在预算内', async () => {
      assert.ok(observed.offQuery && observed.cleanQuery, '对照组或清洗组没有拿到 query');
      const BUDGET = 90_000;
      assert.ok(observed.cleanQuery.length <= BUDGET,
        `清洗后 query 超预算: ${observed.cleanQuery.length}`);
      const delta = Math.abs(observed.cleanQuery.length - observed.offQuery.length);
      // 三条保守改写只补/换几个词，不该出现大段增删
      assert.ok(delta <= 64,
        `清洗前后长度差 ${delta}，超出最小改写应有的范围`);
      // 结构标记不能被动过
      for (const marker of ['[System Instruction]', '[Current Query]', QUESTION]) {
        assert.ok(observed.cleanQuery.includes(marker), `清洗丢了结构标记: ${marker}`);
      }
      // 三条保守改写逐个逆回去，必须与未清洗的 query 逐字一致
      const restore = s => s
        .replace(/(You are (?:an?|the) (?:[\w.-]+\s+){0,3}?(?:agent|assistant|model|AI|LLM)) that is (powered by|built on|built by|created by|developed by|made by|trained by)/gi, '$1 $2')
        .replace(/(Tokens prefixed with @ are paths )that (the user explicitly referenced\.)/gi, '$1$2')
        .replace(/(You are interacting with the user through the [\w ._-]{1,60}? Web GUI) whose URL is named in the runtime context\./gi, '$1 whose URL the runtime context names.');
      assert.equal(restore(observed.cleanQuery), observed.offQuery,
        '清洗不是零信息损失：逆还原后与未清洗的 query 不一致');
    });

    await t.test('aggressive 模式只额外缩品牌词', async () => {
      await withStack({ PROMPT_CLEAN: 'aggressive' }, async ({ api, upstream }) => {
        upstream.onChat({ text: 'ok' });
        const res = await api.chat(chatRequest({
          messages: [{ role: 'system', content: SYSTEM }, { role: 'user', content: 'hi' }],
        }));
        assert.equal(res.status, 200, res.text.slice(0, 300));
        const q = upstream.chatRequests().at(-1).body.query;
        assert.ok(q.includes('mimo-v2.6-pro'), 'aggressive 也不能丢模型信息');
        assert.ok(q.includes('Tokens prefixed with @'), 'aggressive 也不能丢规则');
        assert.ok(!q.includes('DeepSeek Harness'), 'aggressive 应缩掉品牌全名');
      });
    });
  },
});
