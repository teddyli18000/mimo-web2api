#!/usr/bin/env node
// 本地兼容测试入口（同一套 cases，两种上游）。
//
//   # 先跑假上游（不需要任何凭据，应该全绿）
//   node test/compat/run.mjs --mode fake --bin dist/mimo-web2api.exe
//
//   # 再跑真机：只传配置「路径」，内容不会打印、不会写进项目、进程结束即删
//   MIMO_REAL_CONFIG=C:\path\to\config.json \
//     node test/compat/run.mjs --mode real --bin dist/mimo-web2api.exe
//
// 不指定 --bin 时用 MIMO_BIN 环境变量，或仓库根的默认二进制名。
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createFakeDriver, createRealDriver } from './driver.mjs';
import { runCases } from './cases.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');

function parseArgs(argv) {
  const opts = { mode: 'fake', bin: process.env.MIMO_BIN || '', config: process.env.MIMO_REAL_CONFIG || '' };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--mode') opts.mode = argv[++i];
    else if (argv[i] === '--bin') opts.bin = argv[++i];
    else if (argv[i] === '--config') opts.config = argv[++i];
    else throw new Error(`unknown argument: ${argv[i]}`);
  }
  if (!['fake', 'real'].includes(opts.mode)) throw new Error('--mode 只能是 fake 或 real');
  if (!opts.bin) {
    const exe = process.platform === 'win32' ? 'mimo-web2api.exe' : 'mimo-web2api';
    opts.bin = path.join(ROOT, exe);
  }
  opts.bin = path.resolve(opts.bin);
  if (opts.mode === 'fake') process.env.MIMO_BIN = opts.bin;
  return opts;
}

const opts = parseArgs(process.argv.slice(2));
let driver;
const results = [];

const t = {
  async test(name, fn) {
    try {
      await fn();
      results.push({ name, ok: true });
      console.log(`  ✓ ${name}`);
    } catch (err) {
      const msg = driver ? driver.redact(err?.message || String(err)) : String(err?.message || err);
      results.push({ name, ok: false, msg });
      console.log(`  ✗ ${name}\n      ${msg}`);
    }
  },
};

try {
  console.log(`compat mode=${opts.mode}  (凭据不会出现在输出里)`);
  driver = opts.mode === 'fake'
    ? await createFakeDriver()
    : await createRealDriver({ bin: opts.bin, configPath: opts.config });
  await runCases(t, driver);
} catch (err) {
  console.error('compat setup failed:', driver ? driver.redact(err?.message || String(err)) : String(err?.message || err));
  results.push({ name: 'setup', ok: false });
} finally {
  if (driver) await driver.stop().catch(() => {});
}

const failed = results.filter(r => !r.ok);
console.log(`\n${results.length - failed.length} passed, ${failed.length} failed`);
process.exitCode = failed.length ? 1 : 0;
