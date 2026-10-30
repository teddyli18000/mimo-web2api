import assert from 'node:assert/strict';
import { eventually } from './assert.mjs';

// fail-closed 自检：只确认「网关起来了」不够 —— 必须确认它的上游真的被指到假服务。
// 否则 MIMO_TEST_WEB_BASE_URL 一旦失效，整套 e2e 会静默打真实上游、还可能假绿。
// 注意：全程只用 dummy 凭据，且网关子进程里 HTTP(S)_PROXY 已被指向黑洞，
// 即使这里真的配错了，也不会把任何 token 发出去。
export async function assertUpstreamIsLocal(t, { api, upstream }) {
  await t.test('fail-closed：网关的上游确实指向本地假服务', async () => {
    const count = () => upstream.requests.filter(r => r.path === '/open-apis/user/mi/get').length;
    const before = count();
    const res = await api.adminHealth();
    assert.equal(res.status, 200, `health status ${res.status}`);
    await eventually(() => count() > before, {
      timeout: 5000,
      message: '假上游没收到 /open-apis/user/mi/get：MIMO_TEST_WEB_BASE_URL 没生效，测试可能打到了真实上游',
    });
  });
}
