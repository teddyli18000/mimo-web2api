// 一次性排障脚本：打印真机流式响应的每个 content delta，
// 判断回答里的重复片段来自上游生成还是网关拼接。
//
//   node test/compat/check-repeat.mjs <bin> <config-path> [mode]
import { createRealDriver } from './driver.mjs';
import { TOOLS, SYSTEM } from './cases.mjs';

const bin = process.argv[2];
const configPath = process.argv[3] || process.env.MIMO_REAL_CONFIG;
const mode = process.argv[4] || 'conservative';

const driver = await createRealDriver({ bin, configPath, env: { PROMPT_CLEAN: mode } });
try {
  for (let n = 1; n <= 2; n++) {
    const res = await driver.api.chat({
      model: 'mimo-v2.6-pro', tools: TOOLS, stream: true,
      messages: [
        { role: 'system', content: SYSTEM },
        { role: 'user', content: '请用一句话说明 glob 和 read 两个工具分别做什么。' },
      ],
    });
    const deltas = res.text.split(/\r?\n/)
      .filter(l => l.startsWith('data: ') && !l.includes('[DONE]'))
      .map(l => { try { return JSON.parse(l.slice(6)); } catch { return null; } })
      .filter(Boolean)
      .map(c => c.choices?.[0]?.delta?.content || '')
      .filter(Boolean);
    const joined = deltas.join('');
    console.log(`\n=== 第 ${n} 次 (${deltas.length} 个 delta) ===`);
    console.log('deltas:', JSON.stringify(deltas.slice(0, 12)));
    console.log('joined:', JSON.stringify(joined.slice(0, 160)));
    const half = '用于按路径模式查找文件';
    console.log(`重复片段出现次数:`, joined.split(half).length - 1);
  }
} finally {
  await driver.stop();
}
