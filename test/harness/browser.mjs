// 真浏览器里加载真扩展。
//
// 为什么必须用 Playwright 自带的 Chromium：Chrome 137 起 branded Chrome 移除了
// `--load-extension`，只有 Chromium 还能 side-load 未打包扩展。
//
// 为什么 manifest 里要写死 `key`：unpacked 扩展的 ID 是路径哈希，CI 每次临时目录
// 都不同；有了 key 才能算出稳定 ID，直接打开 chrome-extension://<id>/popup.html。
import { createHash } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { chromium } from 'playwright';

export function extensionIdFromKey(keyBase64) {
  const digest = createHash('sha256').update(Buffer.from(keyBase64, 'base64')).digest('hex').slice(0, 32);
  return [...digest].map(c => String.fromCharCode(97 + Number.parseInt(c, 16))).join('');
}

export async function launchWithExtension({ dir, headless = true } = {}) {
  const manifest = JSON.parse(await readFile(path.join(dir, 'manifest.json'), 'utf8'));
  if (!manifest.key) throw new Error('manifest.json 缺少 key，无法固定扩展 ID');
  const id = extensionIdFromKey(manifest.key);

  const userDataDir = await mkdtemp(path.join(tmpdir(), 'mimo-ext-'));
  const context = await chromium.launchPersistentContext(userDataDir, {
    // Playwright 官方文档：channel 'chromium' 才会用完整 Chromium 而不是 headless shell，
    // 扩展在 headless 下也才能加载；缺了它三平台都起不来扩展。
    channel: 'chromium',
    headless,
    args: [
      `--disable-extensions-except=${dir}`,
      `--load-extension=${dir}`,
      '--no-first-run',
      '--no-default-browser-check',
    ],
  });

  return {
    context,
    id,
    popupUrl: `chrome-extension://${id}/popup.html`,

    // 扩展在 CI 里没有真实登录态，往它有 host 权限的域注入 dummy cookie。
    async setMiMoCookies({ serviceToken, userId, ph } = {}) {
      const cookies = [];
      const domain = 'aistudio.xiaomimimo.com';
      if (serviceToken) cookies.push({ name: 'serviceToken', value: serviceToken, domain, path: '/' });
      if (userId) cookies.push({ name: 'userId', value: userId, domain, path: '/' });
      if (ph) cookies.push({ name: 'xiaomichatbot_ph', value: ph, domain, path: '/' });
      await context.addCookies(cookies);
    },

    async clearCookies() {
      await context.clearCookies();
    },

    async close() {
      await context.close();
      await rm(userDataDir, { recursive: true, force: true }).catch(() => {});
    },
  };
}
