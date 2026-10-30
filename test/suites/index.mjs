import { suiteNames } from './manifest.mjs';

// 按 manifest 动态加载，保证「注册表」与「实际加载的套件」不会各说各话。
export const suites = [];
for (const name of suiteNames) {
  const mod = await import(new URL(`./${name}.mjs`, import.meta.url).href);
  const suite = mod.default;
  if (!suite) throw new Error(`suite ${name} has no default export`);
  if (suite.name !== name) throw new Error(`suite ${name}: exported name "${suite.name}" does not match manifest`);
  if (!Array.isArray(suite.tags) || suite.tags.length === 0) throw new Error(`suite ${name}: at least one tag is required`);
  suites.push(suite);
}
