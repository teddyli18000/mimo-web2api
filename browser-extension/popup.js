const GATEWAY_HINT = 'http://localhost:8080';

document.addEventListener('DOMContentLoaded', () => {
  const syncBtn = document.getElementById('syncBtn');
  const statusEl = document.getElementById('status');
  const previewEl = document.getElementById('preview');
  const gatewayEl = document.getElementById('gateway');
  const apikeyEl = document.getElementById('apikey');

  chrome.storage.local.get(['gateway', 'apikey'], (data) => {
    if (data.gateway) gatewayEl.value = data.gateway;
    if (data.apikey) apikeyEl.value = data.apikey;
  });

  syncBtn.addEventListener('click', async () => {
    const gateway = gatewayEl.value.trim().replace(/\/+$/, '');
    const apikey = apikeyEl.value.trim();

    const gatewayError = validateGateway(gateway);
    if (gatewayError) {
      showStatus('err', gatewayError);
      return;
    }

    chrome.storage.local.set({ gateway, apikey });

    syncBtn.disabled = true;
    showStatus('loading', '🔍 正在搜索所有 Cookie...');

    try {
      // 宽泛搜索：先列出所有域名的 cookie
      const allCookies = await chrome.cookies.getAll({});
      const debug = [];
      const mimoCookies = {};

      for (const c of allCookies) {
        const d = c.domain;
        // 匹配所有小米/MiMo 相关域名
        if (d.includes('mimo') || d.includes('xiaomi')) {
          debug.push(`${d} | ${c.name} = ${c.value.slice(0, 30)}...`);

          if (c.name.includes('serviceToken')) {
            mimoCookies.serviceToken = c.value;
          }
          if (c.name === 'userId' || c.name === 'cUserId') {
            if (!mimoCookies.userId) mimoCookies.userId = c.value;
          }
          if (c.name.includes('_ph')) {
            mimoCookies.ph = c.value;
          }
        }
      }

      renderPreview(debug, mimoCookies);

      if (debug.length === 0) {
        showStatus('err', '❌ 未找到任何小米/MiMo Cookie。请先在 aistudio.xiaomimimo.com 登录。');
        return;
      }

      // 三项都是网关调上游的必需品，缺一项写进去也只是个调不通的空壳。
      // 这里先拦下并点名缺哪个，不让它带着空字段发出去。
      const missing = [];
      if (!mimoCookies.serviceToken) missing.push('service_token（serviceToken）');
      if (!mimoCookies.userId) missing.push('user_id（userId）');
      if (!mimoCookies.ph) missing.push('ph（xiaomichatbot_ph）');
      if (missing.length > 0) {
        showStatus('err', `❌ Cookie 不完整，缺少 ${missing.join('、')}。请在 MiMo 对话框先发一条消息后重试。`);
        return;
      }

      showStatus('loading', '📤 正在同步到网关...');

      // id 每次都新生成的，但网关按 user_id 去重：
      // 同一账号重复同步只会覆盖凭据，不会堆出第二个账号。
      const resp = await fetch(`${gateway}/admin/api/accounts`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Authorization': `Bearer ${apikey || 'sk-mimo'}`,
        },
        body: JSON.stringify({
          id: `ext-${Date.now()}`,
          service_token: mimoCookies.serviceToken,
          user_id: mimoCookies.userId,
          ph: mimoCookies.ph,
          active: true,
        }),
      });

      const text = await resp.text();
      if (!resp.ok) {
        // 后端的错误是 {"error":{"code":..,"message":".."}}，把 message 摘出来给人看
        let detail = text;
        try {
          const err = JSON.parse(text);
          if (err && err.error && err.error.message) detail = err.error.message;
        } catch (e) { /* 非 JSON 响应原样展示 */ }
        showStatus('err', `❌ 网关拒绝（${resp.status}）：${detail || '无响应内容'}`);
        return;
      }

      let data = {};
      try {
        data = JSON.parse(text);
      } catch (e) {
        // 老版本网关没有 JSON 响应体，按新增处理
      }
      const id = data && data.id ? `（ID: ${data.id}）` : '';
      const message = data && data.status === 'updated'
        ? `✅ 已更新现有账号${id}，未新增重复项`
        : `✅ 已添加账号${id}`;
      showStatus('ok', `${message}。切回管理面板即可看到。`);

    } catch (e) {
      showStatus('err', `❌ ${e.message}`);
    } finally {
      syncBtn.disabled = false;
    }
  });

  // 只允许本机网关：远端地址不在 host_permissions 内，fetch 会被浏览器拦成 CORS 错误。
  // 提前拦下并说清原因，比让用户对着 "Failed to fetch" 猜要好。
  function validateGateway(raw) {
    let url;
    try {
      url = new URL(raw);
    } catch (e) {
      return `❌ 网关地址格式不对，应形如 ${GATEWAY_HINT}`;
    }
    if (url.protocol !== 'http:' && url.protocol !== 'https:') {
      return '❌ 网关地址只支持 http:// 或 https://';
    }
    const host = url.hostname;
    if (host !== 'localhost' && host !== '127.0.0.1' && host !== '::1' && host !== '[::1]') {
      return '❌ 扩展只同步到本机网关（localhost / 127.0.0.1），其它地址会被浏览器拦截';
    }
    return '';
  }

  function renderPreview(debug, mimoCookies) {
    const lines = [
      `${mimoCookies.serviceToken ? '✅' : '❌'} serviceToken: ${mimoCookies.serviceToken ? mimoCookies.serviceToken.slice(0, 20) + '...' : 'NOT FOUND'}`,
      `${mimoCookies.userId ? '✅' : '❌'} userId: ${mimoCookies.userId || 'NOT FOUND'}`,
      `${mimoCookies.ph ? '✅' : '❌'} ph: ${mimoCookies.ph ? mimoCookies.ph.slice(0, 20) + '...' : 'NOT FOUND'}`,
    ];
    previewEl.innerHTML = lines.map(l => {
      const cls = l.startsWith('✅') ? 'cookie-ok' : 'cookie-miss';
      return `<div class="${cls}">${l}</div>`;
    }).join('') + `<div style="margin-top:6px;color:#555">--- 全部匹配 ---</div>` + debug.slice(0, 10).map(d => `<div style="color:#555">${d}</div>`).join('');
    previewEl.classList.add('show');
  }

  function showStatus(type, msg) {
    statusEl.className = `status ${type}`;
    statusEl.textContent = msg;
  }
});
