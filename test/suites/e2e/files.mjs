// 文本文件附件：走与图片相同的资源接口，但 mediaType=file。
// 断言口径仍然是「上游收到的 multiMedias」，而不是网关返回 200。
import assert from 'node:assert/strict';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest, messagesRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

const TEXT = Buffer.from('mimo file attachment test\n').toString('base64');
const FILE_DATA = `data:text/plain;base64,${TEXT}`;

export default defineSuite({
  name: 'e2e/files',
  tags: ['e2e'],
  async run(t) {
    const stack = await startStack();
    const { api, upstream, gw } = stack;
    try {
      await assertUpstreamIsLocal(t, { api, upstream });

      await t.test('OpenAI：file 内容块进入 multiMedias', async () => {
        upstream.reset();
        upstream.onChat({ text: 'file received' });
        const res = await api.chat(chatRequest({
          messages: [{
            role: 'user',
            content: [
              { type: 'text', text: 'summarize this file' },
              { type: 'file', file: { filename: 'notes.txt', file_data: FILE_DATA } },
            ],
          }],
        }));
        assert.equal(res.status, 200, res.text);

        const medias = upstream.chatRequests().at(-1).body.multiMedias;
        assert.equal(medias.length, 1, JSON.stringify(medias));
        assert.equal(medias[0].mediaType, 'file');
        assert.equal(medias[0].name, 'notes.txt');
        assert.ok(!gw.logText().includes('文本文件上传失败'));
      });

      await t.test('Anthropic：document 块进入 multiMedias', async () => {
        upstream.reset();
        upstream.onChat({ text: 'document received' });
        const res = await api.messages(messagesRequest({
          messages: [{
            role: 'user',
            content: [
              { type: 'text', text: 'summarize this document' },
              { type: 'document', title: 'doc.txt', source: { type: 'base64', media_type: 'text/plain', data: TEXT } },
            ],
          }],
        }));
        assert.equal(res.status, 200, res.text);

        const medias = upstream.chatRequests().at(-1).body.multiMedias;
        assert.equal(medias.length, 1, JSON.stringify(medias));
        assert.equal(medias[0].mediaType, 'file');
        assert.equal(medias[0].name, 'doc.txt');
      });
    } finally {
      await stack.stop();
    }
  },
});
