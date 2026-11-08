// CI 侧：用假上游跑 DSH 形状的兼容性用例。
// 本地真机版见 test/compat/run.mjs（--mode real），两边共用 cases.mjs。
import { defineSuite } from '../../harness/suite.mjs';
import { createFakeDriver } from '../../compat/driver.mjs';
import { runCases } from '../../compat/cases.mjs';

export default defineSuite({
  name: 'e2e/compat-dsh',
  tags: ['e2e'],
  async run(t) {
    const driver = await createFakeDriver();
    try {
      await runCases(t, driver);
    } finally {
      await driver.stop();
    }
  },
});
