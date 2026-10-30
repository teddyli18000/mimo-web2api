// 图片链路：data URL → genUploadInfo → PUT → parse → 绑定到 chat 的 multiMedias。
// 只看「返回 200 且有文字」会假绿 —— handler 上传失败时只打日志然后 continue，
// 所以必须断言上游实际收到的 multiMedias。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

const PNG_A = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';
const PNG_B = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=';
const PNG_C = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==';

const withImage = b64 => chatRequest({
  messages: [{
    role: 'user',
    content: [
      { type: 'text', text: 'describe this image' },
      { type: 'image_url', image_url: { url: `data:image/png;base64,${b64}` } },
    ],
  }],
});

export default defineSuite({
  name: 'e2e/images',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream, gw } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('图片被上传并绑定到 multiMedias', async () => {
        upstream.reset();
        upstream.onChat({ text: 'a tiny image' });
        const res = await api.chat(withImage(PNG_A));
        assert.equal(res.status, 200, res.text);

        const req = upstream.chatRequests().at(-1);
        assert.ok(Array.isArray(req.body.multiMedias), JSON.stringify(req.body.multiMedias));
        assert.equal(req.body.multiMedias.length, 1, '图片没有进 multiMedias，上传链路大概率断了');
        assert.match(String(req.body.multiMedias[0].fileUrl), /\/res\/\d+\.png$/, 'fileUrl 应来自假上游的 parse 结果');

        const log = gw.logText();
        assert.ok(!log.includes('[media] 上传失败'), '网关日志里出现了上传失败');
      });

      await t.test('不同内容的图片会各自上传（缓存不串）', async () => {
        upstream.reset();
        upstream.onChat({ text: 'second image' });
        const res = await api.chat(withImage(PNG_B));
        assert.equal(res.status, 200, res.text);
        assert.equal(upstream.uploadInfoRequests().length, 1, '不同字节的图片应重新走一次 genUploadInfo');
        // 只数上传次数不够：parse 一直失败时上传会被 handler 吞掉，multiMedias 变空
        assert.equal(upstream.chatRequests().at(-1).body.multiMedias.length, 1);
        assert.ok(!gw.logText().includes('[media] 上传失败'));
      });

      await t.test('parse 一直失败时静默降级（现有行为，钉住它）', async () => {
        upstream.reset();
        upstream.setParseFailure(10);
        upstream.onChat({ text: 'no image attached' });
        const res = await api.chat(withImage(PNG_C));
        assert.equal(res.status, 200, res.text);
        assert.equal(upstream.chatRequests().at(-1).body.multiMedias.length, 0, '上传失败时图片不会进 multiMedias');
        assert.ok(gw.logText().includes('[media] 上传失败'), '上传失败目前只打日志，不在响应里体现');
      }, { timeout: 60_000 });
    } finally {
      await stack.stop();
    }
  },
});
