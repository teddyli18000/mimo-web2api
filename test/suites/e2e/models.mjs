// 模型列表：/v1/models 应该来自上游 /open-apis/bot/config，而不是写死的常量。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

export default defineSuite({
  name: 'e2e/models',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('/v1/models 返回上游配置里的模型', async () => {
        const res = await api.get('/v1/models');
        assert.equal(res.status, 200, res.text);
        const ids = (res.json.data || []).map(m => m.id);
        assert.ok(ids.includes('mimo-fake-chat'), JSON.stringify(ids));
        const model = res.json.data.find(m => m.id === 'mimo-fake-chat');
        assert.equal(model.owned_by, 'xiaomi');
      });

      await t.test('管理接口可以强制刷新', async () => {
        upstream.reset();
        const res = await api.post('/admin/api/models/refresh', {});
        assert.equal(res.status, 200, res.text);
        assert.equal(res.json.count, 1, JSON.stringify(res.json));
        assert.equal(upstream.requests.filter(r => r.path === '/open-apis/bot/config').length, 1);
      });
    } finally {
      await stack.stop();
    }
  },
});
