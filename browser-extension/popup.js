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
      const allCookies = await chrome.cookies.getAll({});

      // 小米的登录态横跨 xiaomimimo.com / xiaomi.com / mi.com：
      // userId 常常只在 account.xiaomi.com，而 serviceToken / ph 在 MiMo 自己的域。
      // 所以先按「cookie 名字」找，域名只用来挑优先级，别再靠域名关键词过滤。
      const domainOk = d => /(^|\.)(xiaomimimo\.com|xiaomi\.com|mi\.com)$/i.test(d.replace(/^\./, ''));
      const rank = d => (d.includes('mimo') ? 0 : d.includes('xiaomi') ? 1 : 2);

      const picked = {};
      const relevant = [];
      const take = (key, value, domain) => {
        if (!value) return;
        if (!picked[key] || rank(domain) < rank(picked[key].domain)) picked[key] = { value, domain };
      };

      for (const c of allCookies) {
        if (!domainOk(c.domain)) continue;
        relevant.push(`${c.domain} | ${c.name} = ${c.value.slice(0, 30)}...`);

        const n = c.name.toLowerCase();
        if (n.includes('servicetoken') || n === 'service_token') take('serviceToken', c.value, c.domain);
        if (n === 'userid' || n === 'cuserid') take('userId', c.value, c.domain);
        if (n.endsWith('_ph') || n === 'ph') take('ph', c.value, c.domain);
      }

      const values = {
        serviceToken: picked.serviceToken?.value,
        userId: picked.userId?.value,
        ph: picked.ph?.value,
      };

      renderPreview(relevant, values, allCookies.map(c => `${c.domain} | ${c.name}`));

      if (relevant.length === 0) {
        showStatus('err', '❌ 没读到任何小米域 Cookie。请确认登录 aistudio.xiaomimimo.com 用的是当前浏览器配置；刚更新过扩展的话，先到 chrome://extensions 重新加载。');
        return;
      }

      // 三项都是网关调上游的必需品，缺一项写进去也只是个调不通的空壳。
      // 这里先拦下并点名缺哪个，不让它带着空字段发出去。
      const missing = [];
      if (!values.serviceToken) missing.push('service_token（serviceToken）');
      if (!values.userId) missing.push('user_id（userId）');
      if (!values.ph) missing.push('ph（xiaomichatbot_ph）');
      if (missing.length > 0) {
        showStatus('err', `❌ Cookie 不完整，缺少 ${missing.join('、')}。已读到 ${allCookies.length} 个 cookie，其中小米相关 ${relevant.length} 个；看下方列表确认登录态是否在同一个浏览器配置。`);
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
          service_token: values.serviceToken,
          user_id: values.userId,
          ph: values.ph,
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

  const escapeHtml = s => String(s).replace(/[&<>"']/g, m => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[m]));

  function renderPreview(relevant, values, allNames) {
    const lines = [
      `${values.serviceToken ? '✅' : '❌'} serviceToken: ${values.serviceToken ? values.serviceToken.slice(0, 20) + '...' : 'NOT FOUND'}`,
      `${values.userId ? '✅' : '❌'} userId: ${values.userId || 'NOT FOUND'}`,
      `${values.ph ? '✅' : '❌'} ph: ${values.ph ? values.ph.slice(0, 20) + '...' : 'NOT FOUND'}`,
    ];
    const missing = !values.serviceToken || !values.userId || !values.ph;
    // 缺字段时列出「全部可见 cookie」：用户截一张图就能看出登录态到底在哪个域、叫什么名字
    const list = missing ? allNames : relevant;
    const title = missing
      ? `--- 浏览器可见 cookie（共 ${allNames.length} 个，前 40 条）---`
      : '--- 小米相关 cookie ---';

    previewEl.innerHTML = lines.map(l => {
      const cls = l.startsWith('✅') ? 'cookie-ok' : 'cookie-miss';
      return `<div class="${cls}">${escapeHtml(l)}</div>`;
    }).join('')
      + `<div style="margin-top:6px;color:#555">${title}</div>`
      + list.slice(0, 40).map(d => `<div style="color:#555">${escapeHtml(d)}</div>`).join('');
    previewEl.classList.add('show');
  }

  function showStatus(type, msg) {
    statusEl.className = `status ${type}`;
    statusEl.textContent = msg;
  }
});
