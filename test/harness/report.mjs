// 报告：console（人看）+ JSON / JUnit（CI 看）。
// 一个用例失败不中断同 suite 其余用例 —— 一次跑完拿到完整失败清单比快速失败更有用。

export function createReporter({ suite, stream = process.stdout, onChange }) {
  const cases = [];
  const startedAt = Date.now();

  const push = (status, name, { durationMs = 0, error, reason } = {}) => {
    cases.push({ name, status, durationMs, error: error ? `${error.message}` : undefined, stack: error?.stack, reason });
    // 每个用例结束后就落一次盘：套件被父进程 kill 时，已经跑完的用例不会丢
    onChange?.();
  };

  return {
    pass(name, durationMs = 0) {
      push('passed', name, { durationMs });
      stream.write(`  \x1b[32m✓\x1b[0m ${name} (${durationMs}ms)\n`);
    },
    fail(name, error, durationMs = 0) {
      push('failed', name, { durationMs, error });
      stream.write(`  \x1b[31m✗ ${name}\x1b[0m\n      ${error?.message ?? error}\n`);
    },
    skip(name, reason = 'skipped') {
      push('skipped', name, { reason });
      stream.write(`  \x1b[33m-\x1b[0m ${name} (${reason})\n`);
    },
    result() {
      const failed = cases.filter(c => c.status === 'failed');
      return {
        suite,
        cases,
        total: cases.length,
        passed: cases.filter(c => c.status === 'passed').length,
        failed: failed.length,
        skipped: cases.filter(c => c.status === 'skipped').length,
        durationMs: Date.now() - startedAt,
        ok: failed.length === 0,
      };
    },
  };
}

export function summarize(result) {
  const s = (result.durationMs / 1000).toFixed(1);
  return `${result.suite}: ${result.passed} passed, ${result.failed} failed, ${result.skipped} skipped (${s}s)`;
}

const escapeXml = (s = '') => String(s)
  .replace(/&/g, '&amp;')
  .replace(/</g, '&lt;')
  .replace(/>/g, '&gt;')
  .replace(/"/g, '&quot;')
  .replace(/'/g, '&apos;');

export function toJUnit(result) {
  const time = (result.durationMs / 1000).toFixed(3);
  const cases = result.cases.map(c => {
    const t = (c.durationMs / 1000).toFixed(3);
    if (c.status === 'skipped') {
      return `    <testcase classname="${escapeXml(result.suite)}" name="${escapeXml(c.name)}" time="${t}"><skipped message="${escapeXml(c.reason)}"/></testcase>`;
    }
    if (c.status === 'failed') {
      return `    <testcase classname="${escapeXml(result.suite)}" name="${escapeXml(c.name)}" time="${t}"><failure message="${escapeXml(c.error)}">${escapeXml(c.stack || c.error)}</failure></testcase>`;
    }
    return `    <testcase classname="${escapeXml(result.suite)}" name="${escapeXml(c.name)}" time="${t}"/>`;
  }).join('\n');

  return `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="${escapeXml(result.suite)}" tests="${result.total}" failures="${result.failed}" skipped="${result.skipped}" time="${time}">
${cases}
</testsuite>
`;
}
