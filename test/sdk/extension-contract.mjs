// 模拟浏览器扩展的同步调用（验证扩展与管理接口的契约）
const BASE = process.env.GW || 'http://localhost:8090';
const KEY = 'sk-mimo';

let pass = 0, fail = 0;
const rec = (ok, n, d = '') => { console.log(`  ${ok ? '[PASS]' : '[FAIL]'} ${n}${d ? ' — ' + d : ''}`); ok ? pass++ : fail++; };

async function admin(path, opts = {}) {
  const r = await fetch(BASE + path, {
    ...opts,
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${KEY}`, ...(opts.headers || {}) },
  });
  const text = await r.text();
  let json = null; try { json = JSON.parse(text); } catch {}
  return { status: r.status, json, text };
}

async function countAccounts() {
  const r = await admin('/admin/api/config');
  return r.json?.accounts?.length ?? -1;
}

(async () => {
  console.log('########## 扩展 → 管理接口契约 ##########');

  // user_id 每次运行都不同：上一轮若异常退出留下账号，不会让本轮首同步误判成 updated
  const stamp = Date.now();
  const userId = `999${stamp}`;
  const firstId = `ext-${stamp}`;
  const payload = {
    id: firstId,
    service_token: 'dummy-token-for-contract-test',
    user_id: userId,
    ph: 'dummy-ph==',
    active: true,
  };

  const beforeCount = await countAccounts();
  console.log(`  添加前账号数: ${beforeCount}`);

  // 1. 首次同步：新增
  const add = await admin('/admin/api/accounts', { method: 'POST', body: JSON.stringify(payload) });
  rec(add.status === 200 && add.json?.status === 'added', '首次同步返回 added', `status=${add.status} ${JSON.stringify(add.json)}`);
  rec(add.json?.id === firstId, '生效 ID 与请求一致', `got=${add.json?.id}`);

  const afterAdd = await countAccounts();
  rec(afterAdd === beforeCount + 1, '账号数 +1', `before=${beforeCount} after=${afterAdd}`);

  const cfg = await admin('/admin/api/config');
  const created = (cfg.json?.accounts || []).find(a => a.id === firstId);
  rec(!!created, '账号已写入配置', created ? JSON.stringify({ id: created.id, user_id: created.user_id }) : '未找到');
  rec(created?.service_token === payload.service_token, 'service_token 字段名匹配（无归一化丢失）');
  rec(created?.user_id === payload.user_id, 'user_id 字段名匹配');
  rec(created?.ph === payload.ph, 'ph 字段名匹配');
  rec(created?.active === true, 'active 为 true');

  // 2. 同一账号再同步：id 换了也只更新，不新增
  const again = await admin('/admin/api/accounts', {
    method: 'POST',
    body: JSON.stringify({ ...payload, id: `ext-${stamp}-again`, service_token: 'dummy-token-refreshed' }),
  });
  rec(again.status === 200 && again.json?.status === 'updated', '重复同步返回 updated', `status=${again.status} ${JSON.stringify(again.json)}`);
  rec(again.json?.id === firstId, '保留首次的账号 ID（不跟随来件 id）', `got=${again.json?.id} want=${firstId}`);

  const afterAgain = await countAccounts();
  rec(afterAgain === afterAdd, '账号数不变（不新增重复项）', `afterAdd=${afterAdd} afterAgain=${afterAgain}`);

  const cfg2 = await admin('/admin/api/config');
  const refreshed = (cfg2.json?.accounts || []).find(a => a.id === firstId);
  rec(refreshed?.service_token === 'dummy-token-refreshed', '凭据已更新为新值');
  rec(refreshed?.user_id === userId, 'user_id 保持不变');

  // 3. 缺字段：400，且不落库
  const missing = await admin('/admin/api/accounts', {
    method: 'POST',
    body: JSON.stringify({ id: `ext-${stamp}-bad`, service_token: 'dummy', user_id: `999${stamp}1` }),
  });
  rec(missing.status === 400, '缺 ph 被拒', `status=${missing.status} ${missing.text}`);
  rec((missing.text || '').includes('ph'), '错误信息点名缺 ph', (missing.text || '').trim());
  rec(!(missing.text || '').includes('dummy'), '错误信息不回显凭据值');

  const afterMissing = await countAccounts();
  rec(afterMissing === afterAgain, '被拒的请求没有落库', `after=${afterMissing}`);

  // 4. 省略 active 视为启用
  const noActiveId = `ext-${stamp}-noactive`;
  const noActive = await admin('/admin/api/accounts', {
    method: 'POST',
    body: JSON.stringify({ id: noActiveId, service_token: 'dummy', user_id: `999${stamp}2`, ph: 'dummy-ph' }),
  });
  const cfg3 = await admin('/admin/api/config');
  const createdNoActive = (cfg3.json?.accounts || []).find(a => a.id === noActiveId);
  rec(noActive.status === 200 && createdNoActive?.active === true, '省略 active 时默认启用', JSON.stringify(createdNoActive));
  await admin('/admin/api/accounts', { method: 'DELETE', body: JSON.stringify({ id: noActiveId }) });

  // 5. 健康检查应覆盖新账号（dummy token 会失败，但不该 500）
  const health = await admin('/admin/api/health');
  rec(health.status === 200, '健康检查在新账号加入后仍正常', JSON.stringify(health.json).slice(0, 80));

  // 6. 无鉴权应被拒
  const noAuth = await fetch(BASE + '/admin/api/accounts', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
  rec(noAuth.status === 401, '扩展漏填 Key 时被拒（不会静默失败）', `status=${noAuth.status}`);

  // 清理
  const del = await admin('/admin/api/accounts', { method: 'DELETE', body: JSON.stringify({ id: firstId }) });
  rec(del.status === 200, 'DELETE 清理测试账号', `status=${del.status}`);
  const final = await countAccounts();
  rec(final === beforeCount, '账号数回到测试前', `${final}`);

  console.log(`\n========== 扩展契约: ${pass} PASS / ${fail} FAIL ==========`);
  if (fail > 0) process.exitCode = 1;
})();
