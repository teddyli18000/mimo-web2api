// 套件清单：唯一的注册来源。
// 新增套件 = 加一个文件 + 在这里加一行；meta/registry 会校验两边一致，
// 避免「文件建好了但没人跑」的静默漏测。
export const suiteNames = [
  'meta/registry',
  'contract/accounts',
  'browser/extension',
  'e2e/credentials',
  'e2e/openai',
  'e2e/anthropic',
  'e2e/session',
  'e2e/sse-edges',
  'e2e/images',
  'e2e/tools',
  'e2e/channels',
  'e2e/resilience',
];
