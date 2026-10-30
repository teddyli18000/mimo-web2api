// 真浏览器 + 真扩展 + 真网关：整条「点一下 → cookie 进网关 → 账号池」的链路。
// 浏览器里没有登录态，cookie 用 dummy 值注入；网关侧只走管理接口，不碰上游。
import assert from 'node:assert/strict';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineSuite } from '../../harness/suite.mjs';
import { startGateway } from '../../harness/gateway.mjs';
import { createApi } from '../../harness/http.mjs';
import { launchWithExtension } from '../../harness/browser.mjs';

const EXT_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../../browser-extension');

export default defineSuite({
  name: 'browser/extension',
  tags: ['browser'],
  timeout: 180_000,
  async run(t) {
    const gw = await startGateway({ accounts: [] });
    const api = createApi({ base: gw.base });
    const browser = await launchWithExtension({ dir: EXT_DIR });
    const page = await browser.context.newPage();
    // popup 里的异常不会让 Playwright 报错，只会表现为「点了没反应」，这里显式打出来
    page.on('pageerror', err => console.log(`  [popup error] ${err.message}`));
    page.on('console', msg => {
      if (msg.type() === 'error') console.log(`  [popup console] ${msg.text()}`);
    });

    const openPopup = async () => {
      await page.goto(browser.popupUrl);
      await page.waitForSelector('#syncBtn');
    };

    const sync = async () => {
      await page.fill('#gateway', gw.base);
      await page.fill('#apikey', 'sk-mimo');
      await page.click('#syncBtn');
      await page.waitForSelector('#status.ok, #status.err', { timeout: 20_000 });
      return {
        cls: (await page.getAttribute('#status', 'class')) || '',
        text: (await page.textContent('#status')) || '',
      };
    };

    const accounts = async () => (await api.adminConfig()).json.accounts;

    try {
      await t.test('注入登录态后点一次：账号写进网关', async () => {
        await browser.setMiMoCookies({
          serviceToken: 'dummy-service-token',
          userId: '1234567890',
          ph: 'dummy-ph==',
        });
        await openPopup();

        // 先确认扩展真的看得到注入的 cookie，否则后面只会得到一句「未找到 Cookie」
        const visible = await page.evaluate(async () => {
          const list = await chrome.cookies.getAll({});
          return list.map(c => `${c.domain}|${c.name}`);
        });
        assert.ok(
          visible.some(v => v.includes('serviceToken')),
          `扩展看不到注入的 cookie：${JSON.stringify(visible)}`,
        );

        const result = await sync();

        assert.match(result.cls, /ok/, result.text);
        assert.match(result.text, /已添加/, result.text);

        const list = await accounts();
        assert.equal(list.length, 1, JSON.stringify(list));
        assert.equal(list[0].user_id, '1234567890');
        assert.equal(list[0].ph, 'dummy-ph==');
        assert.equal(list[0].active, true);
      });

      await t.test('再点一次：只更新，不新增、ID 不变', async () => {
        const before = (await accounts())[0];
        await openPopup();
        const result = await sync();

        assert.match(result.text, /已更新/, result.text);
        const list = await accounts();
        assert.equal(list.length, 1, '重复同步不应新增账号');
        assert.equal(list[0].id, before.id, '账号 ID 必须保持稳定');
      });

      await t.test('Cookie 不完整：弹窗点名缺 ph，且不写脏数据', async () => {
        await browser.clearCookies();
        await browser.setMiMoCookies({ serviceToken: 'dummy-service-token', userId: '1234567890' });
        await openPopup();
        const result = await sync();

        assert.match(result.cls, /err/, result.text);
        assert.match(result.text, /ph/, result.text);
        assert.equal((await accounts()).length, 1, '缺字段时不应落库');
      });
    } finally {
      await browser.close();
      await gw.stop();
    }
  },
});
