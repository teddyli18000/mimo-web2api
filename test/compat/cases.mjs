// DSH 场景的同一套用例，fake / real 两种上游共用。
//
// fake：能排上游脚本、能抓上游 query → 精确断言（CI 用这个）
// real：上游是真实模型，不能排脚本 → 只断言「结构必须合法」
//       （工具名在声明列表里、必填参数齐全、id/finish_reason/[DONE] 规范）
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const TOOLS = JSON.parse(readFileSync(path.join(HERE, 'fixtures/dsh-tools.json'), 'utf8'));
export const SYSTEM = readFileSync(path.join(HERE, 'fixtures/dsh-system.txt'), 'utf8');
export const KNOWN = new Set(TOOLS.map(t => t.function.name));

const budget = 90000;

export function requiredOf(name) {
  const t = TOOLS.find(x => x.function.name === name);
  const r = t?.function?.parameters?.required;
  return Array.isArray(r) ? r : [];
}

export function toolText(name, args = {}) {
  const lines = [`<tool_calls>`, `<invoke name="${name}">`];
  for (const [k, v] of Object.entries(args)) {
    lines.push(`<parameter name="${k}">${typeof v === 'string' ? v : JSON.stringify(v)}</parameter>`);
  }
  lines.push('</invoke>', '</tool_calls>');
  return lines.join('\n');
}

export function parseStreamChunks(text) {
  const chunks = text.split(/\r?\n/)
    .filter(l => l.startsWith('data: ') && !l.includes('[DONE]'))
    .map(l => { try { return JSON.parse(l.slice(6)); } catch { return null; } }).filter(Boolean);
  const frags = chunks.flatMap(c => c.choices?.[0]?.delta?.tool_calls || []);
  const args = new Map();
  const names = new Map();
  const ids = new Map();
  for (const f of frags) {
    const i = f.index ?? 0;
    args.set(i, (args.get(i) || '') + (f.function?.arguments || ''));
    if (f.function?.name) names.set(i, f.function.name);
    if (f.id) ids.set(i, f.id);
  }
  const calls = [...args.entries()].map(([i, a]) => ({
    id: ids.get(i) || `call_${i}`,
    function: { name: names.get(i) || '', arguments: a },
  }));
  return {
    calls,
    content: chunks.map(c => c.choices?.[0]?.delta?.content || '').join(''),
    finish: chunks.map(c => c.choices?.[0]?.finish_reason).filter(Boolean),
    ids: [...new Set(chunks.map(c => c.id))],
    done: text.trimEnd().endsWith('[DONE]'),
  };
}

// 结构不变量：两种模式都必须满足。模型可以不调工具，但一旦调了就必须是合法调用。
function assertCallsLegal(calls, label) {
  for (const c of calls) {
    const name = c.function?.name ?? c.name;
    assert.ok(KNOWN.has(name), `${label}: 工具名 ${JSON.stringify(name)} 不在声明的 50 个里`);
    let args;
    try { args = JSON.parse(c.function?.arguments ?? c.arguments ?? '{}'); }
    catch (e) { assert.fail(`${label}: arguments 不是合法 JSON: ${e.message}`); }
    assert.ok(args && typeof args === 'object' && !Array.isArray(args), `${label}: arguments 必须是对象`);
    for (const key of requiredOf(name)) {
      assert.ok(Object.prototype.hasOwnProperty.call(args, key),
        `${label}: ${name} 缺必填参数 "${key}"（DSH 会报 missing required property）`);
    }
    assert.ok(c.id || c.call_id, `${label}: 缺 id/call_id，客户端无法回传结果`);
  }
}

export async function runCases(t, driver) {
  const { api, upstream } = driver;
  const fake = !!upstream;
  const q1 = '先列一下这个仓库根目录有哪些文件。请调用 glob 工具，不要直接用文字回答。';

  const chat = (messages, extra = {}) => api.chat({
    model: 'mimo-v2.6-pro', tools: TOOLS,
    messages: [{ role: 'system', content: SYSTEM }, ...messages],
    ...extra,
  });

  await t.test('无前缀 /models 与 /v1/models 形状一致', async () => {
    const a = await api.get('/models');
    const b = await api.get('/v1/models');
    assert.equal(a.status, 200, a.text.slice(0, 200));
    assert.equal(b.status, 200, b.text.slice(0, 200));
    assert.ok(String(a.headers.get('content-type')).includes('json'), '必须返回 JSON');
    assert.deepEqual((a.json?.data || []).map(m => m.id), (b.json?.data || []).map(m => m.id));
  });

  await t.test('DSH 形状（50 工具 + 长 system）：工具调用参数完整', async () => {
    if (fake) {
      upstream.reset();
      upstream.onChat({ text: toolText('glob', { pattern: '**/*' }) });
    }
    const res = await chat([{ role: 'user', content: q1 }]);
    assert.equal(res.status, 200, res.text.slice(0, 300));
    let calls = res.json?.choices?.[0]?.message?.tool_calls || [];
    assertCallsLegal(calls, '非流式');
    if (fake) {
      assert.equal(calls.length, 1, `期望 1 个调用: ${JSON.stringify(calls)}`);
      assert.equal(calls[0].function.name, 'glob');
      assert.deepEqual(JSON.parse(calls[0].function.arguments), { pattern: '**/*' });
      const query = upstream.chatRequests().at(-1).body.query;
      assert.ok(query.includes('<tool_calls>'), '上游 query 丢了调用契约');
      assert.ok(query.includes('glob(path:string, pattern*:string)'), '上游 query 丢了工具签名');
      assert.ok(query.length <= budget, `query 超预算: ${query.length}`);
    }
  });

  // 这一条是真机暴露的 bug：模型只吐 <invoke name="glob"></invoke>，
  // 网关原样转成 arguments="{}" → 客户端校验必填项直接失败。
  // 修法：检测到缺必填参数时，带纠正提示重问一次上游（仅 fake 能排脚本）。
  if (fake) await t.test('模型漏必填参数：网关自动纠正，不把残缺调用交给客户端', async () => {
    upstream.reset();
    upstream.onChat({ text: toolText('glob') });                       // 第一次：没给 pattern
    upstream.onChat({ text: toolText('glob', { pattern: '**/*' }) }); // 纠正后：给全
    const res = await chat([{ role: 'user', content: q1 }]);
    assert.equal(res.status, 200, res.text.slice(0, 300));
    const calls = res.json?.choices?.[0]?.message?.tool_calls || [];
    assertCallsLegal(calls, '纠正重试');
    assert.equal(calls.length, 1, `应该只有 1 个合法调用: ${JSON.stringify(calls)}`);
    assert.deepEqual(JSON.parse(calls[0].function.arguments), { pattern: '**/*' });
    assert.equal(upstream.chatRequests().length, 2, '应该重问了上游一次');
    assert.ok(upstream.chatRequests().at(-1).body.query.includes('[Correction]'), '纠正提示没进 query');
  });

  // 真机抓到：模型把包装标签和内部闭合写串，配对正则删不干净，
  // `<tool_calls>\n\n</function>` 会作为正文发给客户端。
  if (fake) await t.test('错配闭合的工具标记不会漏进 content', async () => {
    upstream.reset();
    upstream.onChat({ text: '<tool_calls>\n<function=glob>\n<parameter name="pattern">*</parameter>\n</function>\n</function>' });
    const res = await chat([{ role: 'user', content: q1 }]);
    assert.equal(res.status, 200, res.text.slice(0, 300));
    const msg = res.json.choices[0].message;
    const calls = msg.tool_calls || [];
    assertCallsLegal(calls, '畸形闭合');
    assert.equal(calls.length, 1, `应解析出 1 个调用: ${JSON.stringify(calls)}`);
    assert.ok(!String(msg.content || '').includes('<tool_'), `content 残留标签: ${msg.content}`);
    assert.ok(!String(msg.content || '').includes('</function'), `content 残留标签: ${msg.content}`);
  });

  // DSH 实际用 stream:true，前面的用例都是非流式，覆盖不到 SSE 契约
  await t.test('流式 DSH 形状：SSE 契约与 content 无残片', async () => {
    if (fake) {
      upstream.reset();
      upstream.onChat({ text: toolText('glob', { pattern: '**/*' }) });
    }
    const res = await chat([{ role: 'user', content: q1 }], { stream: true });
    assert.equal(res.status, 200, res.text.slice(0, 300));
    const p = parseStreamChunks(res.text);
    assert.equal(p.done, true, `必须以 [DONE] 收尾: ${res.text.slice(-200)}`);
    assert.equal(p.ids.length, 1, `同一个流必须共用 id: ${p.ids.join(',')}`);
    assertCallsLegal(p.calls, '流式');
    assert.ok(!p.content.includes('<tool_') && !p.content.includes('</function'),
      `流式 content 残留标签: ${p.content}`);
    if (fake) {
      assert.deepEqual(p.finish, ['tool_calls']);
      assert.equal(p.calls.length, 1);
      assert.equal(p.calls[0].function.name, 'glob');
      assert.deepEqual(JSON.parse(p.calls[0].function.arguments), { pattern: '**/*' });
    }
  });

  await t.test('工具结果回传：模型能接着往下走', async () => {
    if (fake) {
      upstream.reset();
      upstream.onChat({ text: toolText('glob', { pattern: '**/*' }) });
    }
    const first = await chat([{ role: 'user', content: q1 }]);
    const calls = first.json?.choices?.[0]?.message?.tool_calls || [];
    assertCallsLegal(calls, '第一轮');
    if (!calls.length) return; // real 模式可能直接文字作答，不算失败

    if (fake) upstream.onChat({ text: '已读取，仓库里有 README.md、AGENTS.md、main.go。' });
    const second = await chat([
      { role: 'user', content: q1 },
      { role: 'assistant', content: null, tool_calls: [calls[0]] },
      { role: 'tool', tool_call_id: calls[0].id, content: 'README.md\nAGENTS.md\nmain.go' },
      { role: 'user', content: '请读一下 README.md 前 20 行，调用 read 工具。' },
    ]);
    assert.equal(second.status, 200, second.text.slice(0, 300));
    assertCallsLegal(second.json?.choices?.[0]?.message?.tool_calls || [], '第二轮');
    if (fake) {
      const query = upstream.chatRequests().at(-1).body.query;
      assert.ok(query.includes('[Tool Result]'), `工具结果没回传:\n${query.slice(-800)}`);
      assert.ok(!query.includes(q1), '续轮不该重发完整历史');
      assert.ok(query.includes('read(file_path*:string'), '续轮丢了工具契约');
    }
  });

  if (fake) await t.test('超预算 system：契约不被截断、query 不超上限', async () => {
    upstream.reset();
    upstream.onChat({ text: 'ok' });
    const huge = (SYSTEM + '\n\n[Additional workspace context]\n' + '项目约定与历史记录 '.repeat(300)).repeat(9);
    assert.ok(huge.length > budget, `fixture 没堆过预算: ${huge.length}`);
    const res = await api.chat({
      model: 'mimo-v2.6-pro', tools: TOOLS,
      messages: [{ role: 'system', content: huge }, { role: 'user', content: '列一下目录' }],
    });
    assert.equal(res.status, 200, res.text.slice(0, 300));
    const query = upstream.chatRequests().at(-1).body.query;
    assert.ok(query.length <= budget, `query 超预算: ${query.length}`);
    assert.ok(query.includes('<tool_calls>'), '格式契约被截掉了');
    assert.ok(query.includes('read(file_path*:string'), '工具清单被整段丢掉');
  });

  await t.test('Responses 路径：function_call 项与 call_id', async () => {
    const rTools = TOOLS.slice(0, 12).map(t => ({
      type: 'function', name: t.function.name,
      description: t.function.description || '',
      parameters: t.function.parameters || {},
    }));
    if (fake) upstream.onChat({ text: toolText('read', { file_path: 'README.md' }) });
    const res = await api.post('/v1/responses', {
      model: 'mimo-v2.6-pro', tools: rTools,
      instructions: SYSTEM.slice(0, 1500),
      input: '请调用 read 工具读取 README.md，不要直接回答。',
    });
    assert.equal(res.status, 200, res.text.slice(0, 300));
    const call = (res.json?.output || []).find(o => o.type === 'function_call');
    if (call) {
      assertCallsLegal([{ name: call.name, arguments: call.arguments, call_id: call.call_id }], 'responses');
      assert.ok(call.call_id, '缺 call_id，无法回传 function_call_output');
      assert.equal(call.status, 'completed');
    }
    if (fake) assert.ok(call, `期望 function_call 项: ${JSON.stringify(res.json?.output)}`);
  });
}
