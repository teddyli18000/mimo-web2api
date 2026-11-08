// 双模式测试驱动。
//
//   fake : 复用现有 test/harness（本地假上游 + 临时网关），CI 用这个
//   real : 用本机发布版二进制 + 你自己的 config.json 打真实上游
//
// real 模式只从环境变量/命令行读配置「路径」，配置内容只写进系统临时目录，
// 进程结束立即删除；任何输出都经过 redact()，绝不打印凭据。
import { spawn } from 'node:child_process';
import { copyFile, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { startStack } from '../harness/env.mjs';
import { createApi } from '../harness/http.mjs';
import { getFreePort } from '../harness/gateway.mjs';

const isWindows = process.platform === 'win32';
const sleep = ms => new Promise(r => setTimeout(r, ms));

// redact 抹掉配置里的所有敏感值，避免报错信息把 cookie/key 带出来
function makeRedactor(cfgText) {
  const secrets = [];
  try {
    const cfg = JSON.parse(cfgText);
    if (cfg.api_key) secrets.push(cfg.api_key);
    for (const k of cfg.api_keys || []) secrets.push(k);
    for (const a of cfg.accounts || []) {
      for (const k of ['service_token', 'user_id', 'ph', 'id']) {
        if (a[k]) secrets.push(String(a[k]));
      }
    }
  } catch { /* 配置不可解析时不猜 */ }
  return text => {
    let out = String(text ?? '');
    for (const s of secrets) {
      if (s.length >= 4) out = out.split(s).join('***');
    }
    return out;
  };
}

export async function createFakeDriver() {
  const stack = await startStack();
  return {
    mode: 'fake',
    api: stack.api,
    upstream: stack.upstream,
    redact: s => String(s ?? ''),
    async stop() { await stack.stop(); },
  };
}

export async function createRealDriver({ bin, configPath }) {
  if (!configPath) {
    throw new Error('real 模式需要 --config <path> 或环境变量 MIMO_REAL_CONFIG（只传路径，不要传内容）');
  }
  const cfgText = await readFile(configPath, 'utf8');
  const redact = makeRedactor(cfgText);
  const cfg = JSON.parse(cfgText);
  const apiKey = cfg.api_key || (cfg.api_keys && cfg.api_keys[0]) || 'sk-mimo';

  const dir = await mkdtemp(path.join(tmpdir(), 'mimo-compat-real-'));
  const exe = path.join(dir, isWindows ? 'mimo-web2api.exe' : 'mimo-web2api');
  const cfgDest = path.join(dir, 'config.json');
  await copyFile(bin, exe);
  await writeFile(cfgDest, cfgText, { mode: 0o600 });
  // 账号可能被 secret.key 加密，一并复制（仍在临时目录内）
  try {
    await copyFile(path.join(path.dirname(path.resolve(configPath)), 'secret.key'), path.join(dir, 'secret.key'));
  } catch { /* 明文配置没有这个文件 */ }

  const port = await getFreePort();
  const base = `http://127.0.0.1:${port}`;
  const child = spawn(exe, [], {
    cwd: dir,
    stdio: ['ignore', 'pipe', 'pipe'],
    env: {
      ...process.env,
      CONFIG_PATH: cfgDest,
      PORT: String(port),
      API_KEY: apiKey,
      NO_BROWSER_OPEN: '1',
      MIMO_TEST_WEB_BASE_URL: '',
    },
  });
  let logTail = '';
  const record = chunk => {
    logTail = (logTail + chunk.toString()).slice(-2000);
  };
  child.stdout.on('data', record);
  child.stderr.on('data', record);

  let stopped = false;
  const stop = async () => {
    if (stopped) return;
    stopped = true;
    if (!child.killed) child.kill();
    await Promise.race([new Promise(r => child.once('exit', r)), sleep(5000)]);
    for (let i = 0; i < 5; i++) {
      try { await rm(dir, { recursive: true, force: true }); return; } catch { await sleep(200); }
    }
  };

  let ready = false;
  for (let i = 0; i < 60; i++) {
    await sleep(300);
    try {
      const res = await fetch(`${base}/admin/api/config`, {
        headers: { Authorization: `Bearer ${apiKey}` },
        signal: AbortSignal.timeout(2000),
      });
      if (res.ok) { ready = true; break; }
    } catch { /* 还没起来 */ }
    if (child.exitCode !== null) break;
  }
  if (!ready) {
    await stop();
    throw new Error('真机网关没起来: ' + redact(logTail));
  }

  return {
    mode: 'real',
    // real 模式不能排脚本，给一个静默的占位对象让用例代码不用到处判空
    upstream: null,
    api: createApi({ base, apiKey, timeout: 180_000 }),
    redact,
    // 排障用：网关输出经过 redact，绝不把凭据带出来
    logs: () => redact(logTail),
    stop,
  };
}
