// 日志系统：落盘位置、按天切分、保留期清理、请求 ID 贯穿、凭据不落盘。
//
// 清理是「启动时」和「运行中每 6h」各跑一次，所以这里在 spawn 之前
// 往临时 data/logs 里放种子文件，验证启动那一刻就收敛。
import assert from 'node:assert/strict';
import { access, mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { defineSuite } from '../../harness/suite.mjs';
import { startStack, chatRequest } from '../../harness/env.mjs';
import { assertUpstreamIsLocal } from '../../harness/selfcheck.mjs';

const sleep = ms => new Promise(r => setTimeout(r, ms));

// 网关按本地时区给日志起名，这里也必须用本地日期，不能用 toISOString。
const localDate = d => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
const daysAgo = n => localDate(new Date(Date.now() - n * 86_400_000));
const exists = p => access(p).then(() => true, () => false);

function parseLines(lines) {
  return lines
    .map(l => {
      try { return JSON.parse(l); } catch { return null; }
    })
    .filter(Boolean);
}

export default defineSuite({
  name: 'e2e/logging',
  tags: ['e2e'],
  async run(t) {
    const expired = `${daysAgo(8)}.log`;
    const kept = `${daysAgo(1)}.log`;

    const stack = await startStack({
      gatewayOptions: {
        prepare: async dir => {
          const logDir = path.join(dir, 'data', 'logs');
          await mkdir(logDir, { recursive: true });
          await writeFile(path.join(logDir, expired), '{"msg":"expired"}\n');
          await writeFile(path.join(logDir, kept), '{"msg":"kept"}\n');
        },
      },
    });
    const logDir = path.join(stack.gw.dir, 'data', 'logs');

    try {
      await assertUpstreamIsLocal(t, { api: stack.api, upstream: stack.upstream });

      await t.test('启动时删除超过保留期的日志，保留期内的不动', async () => {
        assert.equal(await exists(path.join(logDir, expired)), false, '8 天前的日志应该被删掉');
        assert.equal(await exists(path.join(logDir, kept)), true, '1 天前的日志应该保留');
        assert.equal(await exists(path.join(logDir, `${localDate(new Date())}.log`)), true, '当天日志文件应该已创建');
      });

      await t.test('X-Request-ID 能追到访问日志', async () => {
        stack.upstream.reset();
        stack.upstream.onChat({ text: 'logged answer' });
        const res = await stack.api.chat(chatRequest());
        assert.equal(res.status, 200, res.text);

        const reqId = res.headers.get('x-request-id');
        assert.ok(reqId, '响应头缺 X-Request-ID');

        let record = null;
        const deadline = Date.now() + 8000;
        while (Date.now() < deadline && !record) {
          const tail = await stack.api.get('/admin/api/logs?lines=400');
          assert.equal(tail.status, 200, tail.text);
          record = parseLines(tail.json?.lines || [])
            .find(r => r.req_id === reqId && r.msg === 'http_request');
          if (!record) await sleep(200);
        }
        assert.ok(record, `日志里没有 req_id=${reqId} 的访问记录`);

        assert.equal(record.path, '/v1/chat/completions');
        assert.equal(record.method, 'POST');
        assert.equal(record.status, 200);
        assert.equal(record.model, 'mimo-v2.6-pro');
        assert.equal(record.account, 'acc-1');
        assert.ok(record.dur_ms >= 0);
      });

      await t.test('日志里没有凭据', async () => {
        const text = await readFile(path.join(logDir, `${localDate(new Date())}.log`), 'utf8');
        assert.ok(!text.includes('dummy-service-token'), '日志里出现了 serviceToken');
        assert.ok(!text.includes('dummy-ph=='), '日志里出现了 ph');
      });
    } finally {
      await stack.stop();
    }
  },
});
