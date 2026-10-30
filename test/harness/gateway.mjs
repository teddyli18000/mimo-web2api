// 启动真实网关二进制。
//
// 三条硬性要求（都来自评审）：
// 1. `data/` 跟着可执行文件走且不可配置（main.go: dataDir = exe 目录/data），
//    所以每个 suite 把二进制复制到自己的临时目录，stats.json / conversations.json 才不会串味。
// 2. 监听失败时网关会 Println + Scanln + exit(1)，stdin 必须是 'ignore'，
//    否则 Scanln 永远等不到 EOF，进程既不退出也不报错，只能等 readiness 超时。
// 3. stdout/stderr 必须持续排空（这里直接进内存环形缓冲），否则 log 写满 pipe 会阻塞网关。
import { spawn } from 'node:child_process';
import { access, chmod, copyFile, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import path from 'node:path';

const isWindows = process.platform === 'win32';

export function defaultBinary() {
  if (process.env.MIMO_BIN) return path.resolve(process.env.MIMO_BIN);
  return path.resolve(isWindows ? 'mimo-web2api.exe' : 'mimo-web2api');
}

export async function getFreePort() {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.unref();
    srv.once('error', reject);
    // 绑 0.0.0.0：网关自己监听的是 ":port"（全网卡），
    // 只在 127.0.0.1 探测会漏掉别的进程已经占了 0.0.0.0:port 的情况。
    srv.listen(0, '0.0.0.0', () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function waitReady(base, apiKey, timeout, isDead) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (isDead()) throw new Error('process exited before becoming ready');
    try {
      const res = await fetch(`${base}/admin/api/config`, {
        headers: { Authorization: `Bearer ${apiKey}` },
        signal: AbortSignal.timeout(1000),
      });
      if (res.ok) return;
      if (res.status === 401) {
        // 别把鉴权失败混进「还没起来」：最常见原因是本地 shell 里有 API_KEY 环境变量，
        // config.go 会用环境变量覆盖 config.json，于是网关的 key 根本不是 sk-mimo。
        throw new Error('gateway responded 401 on /admin/api/config — api key mismatch (API_KEY env leak?)');
      }
    } catch (e) {
      if (e?.message?.includes('api key mismatch')) throw e;
      /* 还没起来 */
    }
    await sleep(100);
  }
  throw new Error(`gateway not ready within ${timeout}ms`);
}

async function removeDir(dir) {
  for (let i = 0; i < 10; i++) {
    try {
      await rm(dir, { recursive: true, force: true });
      return;
    } catch (e) {
      if (!isWindows) throw e;
      await sleep(200); // Windows：子进程刚退出时目录还锁着（EBUSY/EPERM）
    }
  }
}

/**
 * @param {object} opts
 * @param {string} [opts.bin]        网关二进制路径，默认 MIMO_BIN 或仓库根的 mimo-web2api
 * @param {string} [opts.upstream]   假上游 base，会注入 MIMO_TEST_WEB_BASE_URL
 * @param {Array}  [opts.accounts]   写进临时 config.json 的账号
 * @param {boolean}[opts.blockExternal] 默认 true：把 HTTP(S)_PROXY 指向黑洞，误连真实上游会立刻失败而不是把凭据发出去
 */
export async function startGateway({
  bin,
  apiKey = 'sk-mimo',
  accounts = [],
  upstream,
  env = {},
  readyTimeout = 30_000,
  blockExternal = true,
  attempts = 4,
} = {}) {
  const exeSource = bin || defaultBinary();
  await access(exeSource).catch(() => {
    throw new Error(`gateway binary not found: ${exeSource} (build it or set MIMO_BIN)`);
  });

  let lastError;
  for (let attempt = 0; attempt < attempts; attempt++) {
    const dir = await mkdtemp(path.join(tmpdir(), 'mimo-gw-'));
    const exe = path.join(dir, isWindows ? 'mimo-web2api.exe' : 'mimo-web2api');
    const configPath = path.join(dir, 'config.json');
    const logs = [];
    const record = chunk => {
      logs.push(chunk.toString());
      if (logs.length > 4000) logs.splice(0, logs.length - 4000);
    };

    await copyFile(exeSource, exe);
    if (!isWindows) await chmod(exe, 0o755);

    const port = await getFreePort();
    const base = `http://127.0.0.1:${port}`;
    await writeFile(configPath, JSON.stringify({
      port: String(port),
      api_key: apiKey,
      default_model: 'mimo-v2.6-pro',
      accounts,
    }, null, 2));

    const child = spawn(exe, [], {
      cwd: dir,
      stdio: ['ignore', 'pipe', 'pipe'],
      env: {
        ...process.env,
        CONFIG_PATH: configPath,
        PORT: String(port),
        // config.go 会让环境变量覆盖 config.json；本地 shell 里残留的 API_KEY
        // 会让网关换成另一个 key，随后 waitReady 全是 401
        API_KEY: apiKey,
        NO_BROWSER_OPEN: '1',
        // 显式覆盖：父进程环境里残留的值不能漏进 offline 套件
        MIMO_TEST_WEB_BASE_URL: upstream || '',
        // 误配时让出站请求直接失败：宁可红，也不能把 cookie 发到真实上游
        ...(blockExternal ? {
          HTTP_PROXY: 'http://127.0.0.1:9',
          HTTPS_PROXY: 'http://127.0.0.1:9',
          NO_PROXY: '127.0.0.1,localhost,::1',
        } : {}),
        ...env,
      },
    });
    let exited = null;
    let spawnError = null;
    child.stdout.on('data', record);
    child.stderr.on('data', record);
    // spawn 失败（ENOENT / EACCES）只发 'error'，不会触发 'exit'。
    // 不接这个事件的话，等待方会一直以为进程还活着。
    child.on('error', err => { spawnError = err; exited = -1; });

    const exitPromise = new Promise(resolve => {
      child.once('exit', code => { exited = code ?? -1; resolve(code ?? -1); });
    });

    try {
      await waitReady(base, apiKey, readyTimeout, () => exited !== null);
      return {
        base,
        port,
        dir,
        configPath,
        apiKey,
        logs,
        logText: () => logs.join(''),
        async readConfig() {
          return JSON.parse(await readFile(configPath, 'utf8'));
        },
        async stop() {
          if (exited === null) {
            child.kill();
            await Promise.race([exitPromise, sleep(5000)]);
          }
          await removeDir(dir);
        },
      };
    } catch (e) {
      lastError = e;
      if (exited === null) child.kill();
      await Promise.race([exitPromise, sleep(3000)]);
      const text = logs.join('');
      await removeDir(dir);
      // 二进制根本起不来（路径错、没执行位、指向目录）：直接报清楚，不要伪装成「未就绪」
      if (spawnError) {
        throw new Error(`cannot spawn gateway binary ${exe}: ${spawnError.message}`);
      }
      // 端口被抢占时换一个端口重试；其他启动失败直接抛出，带上日志尾巴
      if (/address already in use|Server error/i.test(text) && attempt < attempts - 1) continue;
      throw new Error(`gateway failed to start (port ${port}): ${e.message}\n--- gateway log ---\n${text.slice(-3000)}`);
    }
  }
  throw new Error(`gateway failed to start after ${attempts} attempts: ${lastError?.message}`);
}
