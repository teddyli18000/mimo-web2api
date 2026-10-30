#!/usr/bin/env node
// 统一测试入口。
//
//   node test/run.mjs --list
//   node test/run.mjs --tag offline          # 不需要假上游
//   node test/run.mjs --tag e2e --concurrency 3
//   node test/run.mjs --suite e2e/openai
//
// 每个 suite 默认跑在独立子进程里：Node 侧任何未捕获 rejection、卡住的 socket、
// 没关掉的 server 都只会带走自己那一个 suite，父进程还能给出完整汇总。
// 本地快速迭代可以 --no-isolate 同进程跑。
import { spawn } from 'node:child_process';
import { mkdir, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { createReporter, summarize, toJUnit } from './harness/report.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REGISTRY = path.join(HERE, 'suites', 'index.mjs');
const SELF = fileURLToPath(import.meta.url);

function parseArgs(argv) {
  const opts = {
    tags: [],
    suites: [],
    reportDir: path.join(HERE, '..', 'test-results'),
    concurrency: 1,
    isolate: true,
    list: false,
    child: null,
  };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === '--tag') opts.tags.push(...argv[++i].split(',').map(s => s.trim()).filter(Boolean));
    else if (arg === '--suite') opts.suites.push(...argv[++i].split(',').map(s => s.trim()).filter(Boolean));
    else if (arg === '--report-dir') opts.reportDir = path.resolve(argv[++i]);
    else if (arg === '--concurrency') opts.concurrency = Math.max(1, Number(argv[++i]) || 1);
    else if (arg === '--no-isolate') opts.isolate = false;
    else if (arg === '--list') opts.list = true;
    else if (arg === '--child') opts.child = argv[++i];
    else throw new Error(`unknown argument: ${arg}`);
  }
  return opts;
}

function withTimeout(promise, ms) {
  let timer;
  return Promise.race([
    Promise.resolve(promise).finally(() => clearTimeout(timer)),
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`timeout after ${ms}ms`)), ms);
    }),
  ]);
}

async function executeSuite(suite, reporter) {
  const t = {
    // 软失败：一个用例挂了不影响后面的用例，最后一起汇总
    // 比 http.mjs 的 30s 请求超时更长：让 fetch 的 abort 先落地，报错才是真实原因
    async test(name, fn, { timeout = 45_000 } = {}) {
      const started = Date.now();
      try {
        await withTimeout(Promise.resolve().then(fn), timeout);
        reporter.pass(name, Date.now() - started);
      } catch (e) {
        reporter.fail(name, e, Date.now() - started);
      }
    },
    skip(name, reason = 'skipped') {
      reporter.skip(name, reason);
    },
  };
  await suite.run(t);
}

async function writeResult(reportDir, suite, result) {
  await mkdir(reportDir, { recursive: true });
  const slug = suite.name.replace(/[\\/]/g, '-');
  await writeFile(path.join(reportDir, `${slug}.json`), JSON.stringify(result, null, 2));
  await writeFile(path.join(reportDir, `${slug}.xml`), toJUnit(result));
}

async function runOne(suite, reportDir) {
  let writes = Promise.resolve();
  const reporter = createReporter({
    suite: suite.name,
    // 每个用例结束就增量落盘，套件被超时 kill 时不会连报告一起消失
    onChange: () => {
      const snapshot = reporter.result();
      writes = writes.then(() => writeResult(reportDir, suite, snapshot)).catch(() => {});
    },
  });

  console.log(`\n▶ ${suite.name}  [${suite.tags.join(', ') || 'untagged'}]`);
  try {
    await executeSuite(suite, reporter);
  } catch (e) {
    reporter.fail('<suite>', e);
  }
  const result = reporter.result();
  writes = writes.then(() => writeResult(reportDir, suite, result)).catch(() => {});
  await writes;
  console.log(summarize(result));
  return result;
}

function runChild(suite, reportDir) {
  return new Promise(resolve => {
    let settled = false;
    const finish = payload => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve(payload);
    };

    const child = spawn(process.execPath, [SELF, '--child', suite.name, '--report-dir', reportDir], {
      stdio: 'inherit',
      env: process.env,
    });

    // suite 自己声明的上限之外再留一段收尾时间，防止子进程永远不退出
    const timer = setTimeout(() => {
      child.kill();
      finish({ suite: suite.name, ok: false, failed: 1, note: `killed after ${suite.timeout + 15_000}ms` });
    }, suite.timeout + 15_000);

    // spawn 失败只发 'error' 不发 'exit'，不接这个事件父进程会永久挂起
    child.on('error', e => finish({ suite: suite.name, ok: false, failed: 1, note: `spawn failed: ${e.message}` }));
    child.on('exit', code => finish({
      suite: suite.name,
      ok: code === 0,
      failed: code === 0 ? 0 : 1,
      note: code === 0 ? '' : (code === null ? 'killed by timeout' : `exit ${code}`),
    }));
  });
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  const { suites } = await import(pathToFileURL(REGISTRY).href);

  if (opts.list) {
    for (const s of suites) console.log(`${s.name}\t${s.tags.join(',')}`);
    return;
  }

  if (opts.child) {
    const suite = suites.find(s => s.name === opts.child);
    if (!suite) throw new Error(`unknown suite: ${opts.child}`);
    const result = await runOne(suite, opts.reportDir);
    process.exitCode = result.ok ? 0 : 1;
    return;
  }

  const chosen = suites.filter(s => {
    if (opts.suites.length) return opts.suites.includes(s.name);
    if (opts.tags.length) return s.tags.some(t => opts.tags.includes(t));
    return true;
  });
  if (!chosen.length) {
    console.error(`no suites matched (tags=${opts.tags.join(',') || '-'} suites=${opts.suites.join(',') || '-'})`);
    process.exitCode = 1;
    return;
  }

  console.log(`suites (${chosen.length}): ${chosen.map(s => s.name).join(', ')}`);
  const results = [];

  if (!opts.isolate) {
    for (const suite of chosen) {
      // 同进程模式没有父进程 kill 兜底，这里必须自己套一层超时
      results.push(await withTimeout(runOne(suite, opts.reportDir), suite.timeout + 15_000).catch(e => ({
        suite: suite.name,
        ok: false,
        failed: 1,
        note: e.message,
      })));
    }
  } else {
    const queue = [...chosen];
    const workers = Array.from({ length: Math.min(opts.concurrency, queue.length) }, async () => {
      while (queue.length) {
        const suite = queue.shift();
        results.push(await runChild(suite, opts.reportDir));
      }
    });
    await Promise.all(workers);
  }

  const failed = results.filter(r => !r.ok);
  console.log(`\n${'='.repeat(60)}`);
  console.log(`${results.length} suite(s), ${results.length - failed.length} passed, ${failed.length} failed`);
  for (const r of failed) console.log(`  ✗ ${r.suite}${r.note ? ` — ${r.note}` : ''}`);
  console.log(`reports: ${opts.reportDir}`);
  process.exitCode = failed.length ? 1 : 0;
}

main().catch(e => {
  console.error(e?.stack || e);
  process.exitCode = 1;
});
