// 防止「新增套件文件但忘了注册」的静默漏测：
// manifest 是唯一注册来源，这里把它和磁盘上的文件做双向比对。
import assert from 'node:assert/strict';
import { readdir } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineSuite } from '../../harness/suite.mjs';
import { suiteNames } from '../manifest.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, '..');

async function walk(dir) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...await walk(full));
    else out.push(full);
  }
  return out;
}

export default defineSuite({
  name: 'meta/registry',
  tags: ['offline'],
  async run(t) {
    const onDisk = (await walk(ROOT))
      .map(f => path.relative(ROOT, f).split(path.sep).join('/'))
      .filter(f => f.endsWith('.mjs'))
      .filter(f => f !== 'index.mjs' && f !== 'manifest.mjs')
      .map(f => f.replace(/\.mjs$/, ''));

    await t.test('manifest 与磁盘文件一一对应', () => {
      assert.deepEqual([...suiteNames].sort(), onDisk.sort());
    });
  },
});
