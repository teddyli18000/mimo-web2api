// 一套「真网关 + 假上游」的运行环境。
// 只有上游被替换；网关二进制、鉴权、config.json、SSE 解析都是真实路径。
import { startFakeUpstream } from './fake-upstream.mjs';
import { startGateway } from './gateway.mjs';
import { createApi } from './http.mjs';

export function dummyAccount(overrides = {}) {
  return {
    id: 'acc-1',
    service_token: 'dummy-service-token',
    user_id: '1234567890',
    ph: 'dummy-ph==',
    active: true,
    ...overrides,
  };
}

export async function startStack({ accounts = [dummyAccount()], upstreamOptions, gatewayOptions } = {}) {
  const upstream = await startFakeUpstream(upstreamOptions);
  let gw;
  try {
    gw = await startGateway({ upstream: upstream.base, accounts, ...gatewayOptions });
  } catch (e) {
    await upstream.stop();
    throw e;
  }
  const api = createApi({ base: gw.base, apiKey: gw.apiKey });

  return {
    upstream,
    gw,
    api,
    async stop() {
      await gw.stop();
      await upstream.stop();
    },
  };
}

export function chatRequest(overrides = {}) {
  return {
    model: 'mimo-v2.6-pro',
    messages: [{ role: 'user', content: 'hi' }],
    ...overrides,
  };
}

export function messagesRequest(overrides = {}) {
  return {
    model: 'mimo-v2.6-pro',
    max_tokens: 256,
    messages: [{ role: 'user', content: 'hi' }],
    ...overrides,
  };
}
