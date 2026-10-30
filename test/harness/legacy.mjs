// 适配 test/sdk 里那批「真实上游」脚本。
//
// 这批脚本不能进 CI（要真实账号、消耗额度），但它们仍有人在本地跑，
// 所以这里只做一层薄封装，解决两个历史坑：
// 1. 18/19 个脚本失败也返回 0 —— 不能信退出码，必须解析汇总行/判定行；
// 2. 汇总行 label 各不相同 —— 只认 `N PASS / M FAIL` 这个形状。
import { spawn } from 'node:child_process';
import path from 'node:path';

const SUMMARY_RE = /^=+\s*[^:=]+:\s*(\d+)\s*PASS\s*\/\s*(\d+)\s*FAIL\s*=+\s*$/m;

export async function runLegacy({
  script,
  cwd = path.resolve('test/sdk'),
  gw = process.env.GW || 'http://localhost:8090',
  timeout = 30 * 60_000,
  echo = false,
} = {}) {
  if (!script) throw new Error('runLegacy: script is required');
  const full = path.resolve(cwd, script);
  const child = spawn(process.execPath, [full, ...process.argv.slice(2)], {
    cwd,
    env: { ...process.env, GW: gw },
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  let output = '';
  child.stdout.on('data', d => { output += d; if (echo) process.stdout.write(d); });
  child.stderr.on('data', d => { output += d; if (echo) process.stderr.write(d); });

  const timer = setTimeout(() => child.kill(), timeout);
  const code = await new Promise(resolve => child.on('exit', c => resolve(c)));
  clearTimeout(timer);

  const summary = output.match(SUMMARY_RE);
  const pass = summary ? Number(summary[1]) : (output.match(/\[PASS\]/g) || []).length;
  const fail = summary ? Number(summary[2]) : (output.match(/\[FAIL\]/g) || []).length;

  // 没有汇总行、没有判定行、或者进程崩了：一律算失败。
  // 现状是「脚本在第一个 await 抛错」时既没有 FAIL 也没有非零退出码，最容易被误判成通过。
  const ok = code === 0 && fail === 0 && pass > 0 && Boolean(summary || /\[PASS\]|\[FAIL\]/.test(output));

  return { script, code, pass, fail, summary: Boolean(summary), ok, output };
}
