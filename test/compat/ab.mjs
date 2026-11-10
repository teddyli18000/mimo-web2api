#!/usr/bin/env node
// 真机 A/B：同一请求分别跑 PROMPT_CLEAN=off 与默认 conservative，
// 比对工具调用实参、finish_reason、回答形状，确认「清洗没有改变输出质量」。
//
//   node test/compat/ab.mjs <bin> <config-path>
//
// 两个网关各用随机端口、各自的配置副本（系统临时目录），跑完删除；
// 输出只含结构信息，不含任何配置内容或凭据。
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRealDriver } from './driver.mjs';
import { TOOLS, SYSTEM, parseStreamChunks } from './cases.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const bin = process.argv[2] || process.env.MIMO_BIN || path.join(ROOT, process.platform === 'win32' ? 'mimo-web2api.exe' : 'mimo-web2api');
const configPath = process.argv[3] || process.env.MIMO_REAL_CONFIG;

const q1 = '请调用 glob 工具列出这个仓库根目录的文件，不要直接用文字回答。';
const q2 = '请调用 read 工具读取 README.md 的前 20 行，不要直接用文字回答。';

const body = (messages, stream = false) => ({
  model: 'mimo-v2.6-pro', tools: TOOLS, stream,
  messages: [{ role: 'system', content: SYSTEM }, ...messages],
});

async function run(driver, label) {
  const out = { label, steps: [] };
  const t0 = Date.now();
  try {
    const r1 = await driver.api.chat(body([{ role: 'user', content: q1 }]));
    const c1 = r1.json?.choices?.[0]?.message?.tool_calls || [];
    out.steps.push({ step: 'glob', status: r1.status, finish: r1.json?.choices?.[0]?.finish_reason,
      calls: c1.map(c => ({ name: c.function.name, args: c.function.arguments })) });
    if (!c1.length) return out;

    const r2 = await driver.api.chat(body([
      { role: 'user', content: q1 },
      { role: 'assistant', content: null, tool_calls: [c1[0]] },
      { role: 'tool', tool_call_id: c1[0].id, content: 'README.md\nAGENTS.md\nmain.go' },
      { role: 'user', content: q2 },
    ]));
    const c2 = r2.json?.choices?.[0]?.message?.tool_calls || [];
    out.steps.push({ step: 'read', status: r2.status, finish: r2.json?.choices?.[0]?.finish_reason,
      calls: c2.map(c => ({ name: c.function.name, args: c.function.arguments })) });

    // 任务型提问，不用自指（身份句被改写时自指回答本来就该不同，会污染对比）
    const r3 = await driver.api.chat(body([{ role: 'user', content: '请用一句话说明 glob 和 read 两个工具分别做什么。' }], true));
    const p3 = parseStreamChunks(r3.text);
    out.steps.push({ step: 'stream', status: r3.status, done: p3.done, ids: p3.ids.length,
      finish: p3.finish.join(','), textLen: p3.content.length, textHead: p3.content.slice(0, 80) });
  } catch (err) {
    out.error = driver.redact(err?.message || String(err));
  }
  out.ms = Date.now() - t0;
  return out;
}

const drivers = [];
const results = [];
try {
  for (const mode of ['off', 'conservative']) {
    const driver = await createRealDriver({ bin, configPath, env: { PROMPT_CLEAN: mode } });
    drivers.push(driver);
    const result = await run(driver, mode);
    console.log(`\n=== PROMPT_CLEAN=${mode} (${result.ms}ms) ===`);
    if (result.error) console.log('  ERROR:', result.error);
    for (const s of result.steps) console.log(' ', JSON.stringify(s));
    results.push(result);
  }
  const [a, b] = results;
  console.log('\n=== 输出质量对比 ===');
  const norm = arr => arr.map(s => ({
    step: s.step, status: s.status, finish: s.finish,
    names: (s.calls || []).map(c => c.name),
    argKeys: (s.calls || []).map(c => { try { return Object.keys(JSON.parse(c.args)).sort().join(','); } catch { return 'INVALID_JSON'; } }),
    done: s.done, ids: s.ids,
  }));
  const na = norm(a.steps), nb = norm(b.steps);
  for (let i = 0; i < Math.max(na.length, nb.length); i++) {
    const ok = JSON.stringify(na[i]) === JSON.stringify(nb[i]);
    console.log(`  step${i + 1}: ${ok ? 'MATCH' : 'DIFF'}  off=${JSON.stringify(na[i])}  clean=${JSON.stringify(nb[i])}`);
  }
  const allMatch = na.length === nb.length && na.every((x, i) => JSON.stringify(x) === JSON.stringify(nb[i]));
  console.log(allMatch ? '\n结论：结构完全一致（工具名/参数键/finish/流式契约）' : '\n结论：结构有差异，需排查');
  process.exitCode = allMatch ? 0 : 1;
} finally {
  for (const d of drivers) await d.stop().catch(() => {});
}
