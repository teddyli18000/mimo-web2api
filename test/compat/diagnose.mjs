#!/usr/bin/env node
// 真机诊断：把每次工具调用的 name / arguments 打出来，确认用例真的覆盖到链路。
// 只输出结构，不输出任何配置内容或凭据。
//
//   node test/compat/diagnose.mjs <bin> <config-path>
//
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRealDriver } from './driver.mjs';
import { TOOLS, SYSTEM, parseStreamChunks } from './cases.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const bin = process.argv[2] || process.env.MIMO_BIN || path.join(ROOT, process.platform === 'win32' ? 'mimo-web2api.exe' : 'mimo-web2api');
const configPath = process.argv[3] || process.env.MIMO_REAL_CONFIG;

const driver = await createRealDriver({ bin, configPath });
try {
  const q1 = '先列一下这个仓库根目录有哪些文件。请调用 glob 工具，不要直接用文字回答。';
  const body = (messages, extra = {}) => ({
    model: 'mimo-v2.6-pro', tools: TOOLS,
    messages: [{ role: 'system', content: SYSTEM }, ...messages],
    ...extra,
  });

  // 每个请求独立计时、独立兜错：一个卡住不能把整份诊断带走
  async function send(label, reqBody, isStream) {
    const t0 = Date.now();
    try {
      const res = await driver.api.chat(reqBody);
      const parsed = isStream ? parseStreamChunks(res.text) : null;
      const calls = parsed?.calls || res.json?.choices?.[0]?.message?.tool_calls || [];
      const content = parsed?.content || res.json?.choices?.[0]?.message?.content || '';
      console.log(`\n[${label}] ${Date.now() - t0}ms status=${res.status} calls=${calls.length}`);
      for (const c of calls) {
        console.log('   call:', c.function?.name ?? c.name,
          '| args:', c.function?.arguments ?? c.arguments,
          '| id:', c.id || c.call_id);
      }
      console.log('   content:', JSON.stringify(String(content).slice(0, 120)));
      console.log('   content-type:', res.headers.get('content-type'),
        '| body bytes:', res.text.length,
        '| raw head:', JSON.stringify(res.text.slice(0, 240)));
      if (parsed) {
        console.log('   finish:', parsed.finish.join(','), '| ids:', parsed.ids.length,
          '| [DONE]:', parsed.done);
      }
      return { res, parsed, calls };
    } catch (err) {
      console.log(`\n[${label}] FAILED after ${Date.now() - t0}ms:`, driver.redact(err?.message || String(err)));
      console.log('   gateway log tail:', driver.logs().slice(-1200));
      return { res: null, parsed: null, calls: [] };
    }
  }

  const a = await send('轮1 列目录 非流式', body([{ role: 'user', content: q1 }]), false);
  const b = await send('轮1 列目录 流式', body([{ role: 'user', content: q1 }], { stream: true }), true);

  const first = a.calls[0] || b.parsed?.calls?.[0];
  if (first) {
    const c = await send('轮2 读文件', body([
      { role: 'user', content: q1 },
      { role: 'assistant', content: null, tool_calls: [first] },
      { role: 'tool', tool_call_id: first.id, content: 'README.md\nAGENTS.md\nmain.go' },
      { role: 'user', content: '请读一下 README.md 的前 20 行，调用 read 工具。' },
    ]), false);

    const second = c.calls[0];
    if (second) {
      await send('轮3 收尾', body([
        { role: 'user', content: q1 },
        { role: 'assistant', content: null, tool_calls: [first] },
        { role: 'tool', tool_call_id: first.id, content: 'README.md\nAGENTS.md\nmain.go' },
        { role: 'user', content: '请读一下 README.md 的前 20 行，调用 read 工具。' },
        { role: 'assistant', content: null, tool_calls: [second] },
        { role: 'tool', tool_call_id: second.id, content: '# MiMo Web2API\n\n把 MiMo 网页会话转成 OpenAI 兼容 API 的本地网关。' },
      ]), false);
    }
  }
} finally {
  await driver.stop();
}
