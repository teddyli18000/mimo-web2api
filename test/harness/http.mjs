// 网关客户端：只用 Node 20 内置 fetch，不引第三方依赖。
// 每个请求都有硬超时 —— 网关内部有 30 分钟超时，CI 不能等它。

export function createApi({ base, apiKey = 'sk-mimo', timeout = 30_000 } = {}) {
  if (!base) throw new Error('createApi: base is required');

  async function request(path, { method = 'GET', body, headers = {}, auth = true, timeout: override } = {}) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), override ?? timeout);
    try {
      const res = await fetch(base + path, {
        method,
        headers: {
          ...(auth ? { Authorization: `Bearer ${apiKey}` } : {}),
          ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
          ...headers,
        },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: controller.signal,
      });
      const text = await res.text();
      let json = null;
      try { json = JSON.parse(text); } catch { /* SSE 或空响应 */ }
      return { status: res.status, ok: res.ok, text, json, headers: res.headers };
    } finally {
      clearTimeout(timer);
    }
  }

  return {
    base,
    request,
    get: (path, opts) => request(path, opts),
    post: (path, body, opts) => request(path, { ...opts, method: 'POST', body }),
    del: (path, body, opts) => request(path, { ...opts, method: 'DELETE', body }),
    chat: (body, opts) => request('/v1/chat/completions', { ...opts, method: 'POST', body }),
    messages: (body, opts) => request('/v1/messages', { ...opts, method: 'POST', body }),
    adminConfig: (opts) => request('/admin/api/config', opts),
    adminHealth: (opts) => request('/admin/api/health', { timeout: 10_000, ...opts }),
    addAccount: (account, opts) => request('/admin/api/accounts', { ...opts, method: 'POST', body: account }),
    deleteAccount: (id, opts) => request('/admin/api/accounts', { ...opts, method: 'DELETE', body: { id } }),
  };
}
