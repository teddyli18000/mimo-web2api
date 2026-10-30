// 管理接口契约：扩展发什么、网关就该按什么语义落库。
// 全程 dummy 凭据、不配假上游 —— 这个套件连外网都不能碰。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startGateway } from '../../harness/gateway.mjs';
import { createApi } from '../../harness/http.mjs';

const account = (over = {}) => ({
  id: 'acc-1',
  service_token: 'dummy-token',
  user_id: 'u1',
  ph: 'dummy-ph==',
  active: true,
  ...over,
});

export default defineSuite({
  name: 'contract/accounts',
  tags: ['offline'],
  async run(t) {
    // 预置两条同 user_id 的历史脏数据，验证 upsert 会顺手收敛
    const gw = await startGateway({
      accounts: [account({ id: 'old-1' }), account({ id: 'old-2' })],
    });
    try {
      const api = createApi({ base: gw.base });
      const list = async () => (await api.adminConfig()).json.accounts;

      await t.test('同 user_id 再同步：保留首个 ID 并收敛历史重复行', async () => {
        const res = await api.addAccount(account({ id: 'new-1', service_token: 'tok-new' }));
        assert.equal(res.status, 200, res.text);
        assert.deepEqual(res.json, { status: 'updated', id: 'old-1' });

        const mine = (await list()).filter(a => a.user_id === 'u1');
        assert.equal(mine.length, 1, '两条同 user_id 的历史行应收敛为一条');
        assert.equal(mine[0].id, 'old-1');
        assert.equal(mine[0].service_token, 'tok-new');
      });

      await t.test('新 user_id 返回 added', async () => {
        const res = await api.addAccount(account({ id: 'acc-2', user_id: 'u2' }));
        assert.equal(res.status, 200, res.text);
        assert.deepEqual(res.json, { status: 'added', id: 'acc-2' });
      });

      await t.test('换 id 同步同一账号仍是 updated，且账号数不变', async () => {
        const before = (await list()).length;
        const res = await api.addAccount(account({ id: 'ext-999', user_id: 'u2', service_token: 'tok-2' }));
        assert.deepEqual(res.json, { status: 'updated', id: 'acc-2' });
        const after = await list();
        assert.equal(after.length, before);
        assert.equal(after.find(a => a.user_id === 'u2').service_token, 'tok-2');
      });

      await t.test('缺 ph 返回 400、不落库、不回显凭据', async () => {
        const before = (await list()).length;
        const res = await api.addAccount({
          id: 'bad-1',
          service_token: 'super-secret-token',
          user_id: 'u3',
        });
        assert.equal(res.status, 400, res.text);
        assert.ok(res.text.includes('ph'), `错误信息应点名 ph: ${res.text}`);
        assert.ok(!res.text.includes('super-secret-token'), '错误信息不得回显凭据');
        assert.equal((await list()).length, before);
      });

      await t.test('省略 active 时默认启用', async () => {
        const res = await api.addAccount(account({ id: 'acc-3', user_id: 'u4', active: undefined }));
        assert.equal(res.status, 200, res.text);
        const created = (await list()).find(a => a.user_id === 'u4');
        assert.equal(created.active, true);
      });

      await t.test('缺少 API Key 返回 401', async () => {
        const res = await api.addAccount(account({ id: 'nope', user_id: 'u5' }), { auth: false });
        assert.equal(res.status, 401);
      });

      await t.test('删除按 ID 生效', async () => {
        const del = await api.deleteAccount('acc-2');
        assert.equal(del.status, 200, del.text);
        assert.ok(!(await list()).some(a => a.id === 'acc-2'));
      });

      await t.test('落盘 config.json 与接口返回一致', async () => {
        const fromApi = await list();
        const fromDisk = (await gw.readConfig()).accounts;
        assert.deepEqual(fromDisk, fromApi);
      });
    } finally {
      await gw.stop();
    }
  },
});
