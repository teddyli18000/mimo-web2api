// 用例注册：suite 只声明 name/tags/timeout/run，执行与报告交给 runner。
// name 同时是文件相对路径（test/suites/<name>.mjs），meta 套件靠这个约定查漏注册。
export function defineSuite(spec) {
  if (!spec || typeof spec.name !== 'string' || !spec.name) throw new Error('defineSuite: name is required');
  if (typeof spec.run !== 'function') throw new Error(`defineSuite(${spec.name}): run is required`);
  return {
    tags: [],
    // 整个套件的硬上限（父进程据此 kill 子进程）。单个用例默认 45s，
    // 一个套件常有 4-6 个用例，60s 会在慢机器上把套件拦腰砍断。
    timeout: 180_000,
    ...spec,
  };
}
