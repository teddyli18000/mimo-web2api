// 假 MiMo 上游：网关之外的所有东西都是真的，只有这一层被换成本地服务器。
// 端点形状来自 internal/mimo/web_client.go 与 media.go；请求全部记录，供用例断言
// 「网关到底发了什么」——只看网关响应会漏掉「上传失败被吞」这类假绿。
import { createServer } from 'node:http';
import { frame } from './sse.mjs';

const CHAT_PATHS = new Set(['/open-apis/bot/chat', '/fastchat/open-apis/bot/chat']);
const sleep = ms => new Promise(r => setTimeout(r, ms));

const json = (res, code, obj) => {
  res.writeHead(code, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(obj));
};

const parseJson = raw => {
  if (!raw.length) return null;
  try { return JSON.parse(raw.toString('utf8')); } catch { return null; }
};

function route(req, res, url, record, state) {
  const p = url.pathname;

  if (req.method === 'GET' && p === '/open-apis/user/mi/get') {
    return json(res, 200, { code: 0 });
  }
  if (req.method === 'POST' && CHAT_PATHS.has(p)) {
    return chat(req, res, state);
  }
  if (req.method === 'POST' && p.endsWith('/open-apis/chat/conversation/save')) {
    return json(res, 200, { code: 0 });
  }
  if (req.method === 'POST' && p === '/open-apis/resource/genUploadInfo') {
    const n = ++state.mediaSeq;
    return json(res, 200, {
      code: 0,
      data: {
        uploadUrl: `${state.base}/upload/${n}`,
        resourceUrl: `${state.base}/res/${n}.png`,
        objectName: `obj/${n}.png`,
      },
    });
  }
  if (req.method === 'PUT' && p.startsWith('/upload/')) {
    res.writeHead(200, { 'Content-Type': 'text/plain' });
    return res.end('ok');
  }
  if (req.method === 'POST' && p === '/open-apis/resource/parse') {
    if (state.parseFails > 0) {
      state.parseFails--;
      return json(res, 200, { code: 1, msg: 'parse failed by script' });
    }
    return json(res, 200, { code: 0, data: { id: `media_${++state.mediaSeq}`, tokenUsage: 12 } });
  }

  json(res, 404, { code: 404, msg: `unexpected ${req.method} ${p}` });
}

async function chat(req, res, state) {
  let script;
  if (state.chatQueue.length) {
    script = state.chatQueue.shift();
  } else if (state.chatDefault) {
    script = state.chatDefault;
  } else {
    // 队列空 = 用例忘了排脚本。静默回落到默认文本会让「脚本没排上」变成绿，
    // 所以这里直接 400（网关不重试 4xx，失败很快）。
    state.scriptMisses++;
    res.writeHead(400, { 'Content-Type': 'application/json' });
    return res.end(JSON.stringify({ code: 400, msg: 'fake upstream: no chat script queued' }));
  }
  if (script.delayMs) await sleep(script.delayMs);

  if (script.status) {
    res.writeHead(script.status, { 'Content-Type': 'application/json' });
    return res.end(JSON.stringify(script.body ?? { code: script.status, msg: 'scripted upstream error' }));
  }

  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'keep-alive' });

  if (script.raw !== undefined) return res.end(script.raw);

  let out = '';
  const id = script.id || `msg_${++state.seq}`;
  if (script.events) {
    for (const ev of script.events) out += frame(ev);
  } else {
    if (script.text !== undefined && script.text !== '') {
      out += frame({ id, event: 'message', data: { type: 'text', content: script.text } });
    }
    if (script.error !== undefined) {
      out += frame({ event: 'error', data: { type: 'error', content: script.error } });
    }
    if (script.usage !== false) {
      out += frame({
        event: 'usage',
        data: script.usage || { promptTokens: 12, completionTokens: 7, totalTokens: 19 },
      });
    }
  }
  res.end(out);
}

export async function startFakeUpstream({ defaultText } = {}) {
  const state = {
    base: '',
    requests: [],
    chatQueue: [],
    // 显式默认值：只有用例自己 setChatDefault 时才回落，否则「没排脚本」要报错
    chatDefault: defaultText === undefined ? null : { text: defaultText },
    mediaSeq: 0,
    seq: 0,
    scriptMisses: 0,
    parseFails: 0,
  };

  const server = createServer((req, res) => {
    const chunks = [];
    req.on('data', c => chunks.push(c));
    req.on('end', () => {
      const raw = Buffer.concat(chunks);
      const url = new URL(req.url, 'http://127.0.0.1');
      const record = {
        method: req.method,
        path: url.pathname,
        query: Object.fromEntries(url.searchParams),
        headers: req.headers,
        body: parseJson(raw),
        rawBody: raw.toString('utf8'),
      };
      state.requests.push(record);
      try {
        route(req, res, url, record, state);
      } catch (e) {
        json(res, 500, { code: 500, msg: String(e) });
      }
    });
  });

  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  state.base = `http://127.0.0.1:${server.address().port}`;

  const filter = p => state.requests.filter(r => p(r));

  return {
    get base() { return state.base; },
    get requests() { return state.requests; },
    chatRequests: () => filter(r => CHAT_PATHS.has(r.path)),
    saveRequests: () => filter(r => r.path.endsWith('/open-apis/chat/conversation/save')),
    uploadInfoRequests: () => filter(r => r.path === '/open-apis/resource/genUploadInfo'),
    parseRequests: () => filter(r => r.path === '/open-apis/resource/parse'),
    get scriptMisses() { return state.scriptMisses; },
    /** 排一条 chat 响应；每来一个 chat 请求按 FIFO 取一条 */
    onChat(script) { state.chatQueue.push(script); },
    setChatDefault(script) { state.chatDefault = script; },
    /** 让接下来 n 次 resource/parse 返回业务失败（验证「上传失败被吞」的现有行为） */
    setParseFailure(n) { state.parseFails = n; },
    reset() { state.requests.length = 0; state.chatQueue.length = 0; },
    async waitFor(predicate, { timeout = 5000, message = 'condition not met' } = {}) {
      const deadline = Date.now() + timeout;
      while (Date.now() < deadline) {
        if (await predicate()) return;
        await sleep(50);
      }
      throw new Error(message);
    },
    async stop() {
      await new Promise(resolve => {
        server.close(() => resolve());
        // 网关进程若没在 5s 内被 kill 干净，keep-alive 连接会让 close 永远不回调
        server.closeAllConnections?.();
      });
    },
  };
}
