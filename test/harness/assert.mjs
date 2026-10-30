import assert from 'node:assert/strict';

export { assert };

// 轮询等待：用于「网关异步发起 save / 健康检查」这类 fire-and-forget 行为。
// 不用固定 sleep —— 固定 sleep 在慢机器上会假红，在快机器上白等。
export async function eventually(fn, { timeout = 5000, interval = 50, message = 'condition not met in time' } = {}) {
  const deadline = Date.now() + timeout;
  let lastError;
  while (Date.now() < deadline) {
    try {
      const value = await fn();
      if (value) return value;
    } catch (e) {
      lastError = e;
    }
    await new Promise(r => setTimeout(r, interval));
  }
  if (lastError) throw new Error(`${message} (last error: ${lastError.message})`);
  throw new Error(message);
}

export function sleep(ms) {
  return new Promise(r => setTimeout(r, ms));
}
