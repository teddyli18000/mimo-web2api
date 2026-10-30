# 测试框架

分三层，按「需不需要真实上游」切分：

| 层 | tag | 跑的是什么 | 凭据 | 在哪跑 |
|---|---|---|---|---|
| 单元 | — | `go test ./...` | 无 | CI `build` job |
| 契约 / E2E | `offline` / `e2e` | 真实网关二进制 + 本地假上游 | **dummy 值**，零真实 token | CI `offline` / `e2e` job |
| 浏览器 | `browser` | 真 Chromium 加载真扩展，点弹窗把 cookie 同步进真网关 | dummy cookie | CI `browser` job（xvfb） |
| 真实上游 | — | [`test/sdk/`](sdk/README.md) 里的 19 个脚本 | 自己的小米账号 | 只在本地，人工触发 |

跑在 CI 的两层不会访问 `aistudio.xiaomimimo.com`：上游被 `MIMO_TEST_WEB_BASE_URL` 指向 `test/harness/fake-upstream.mjs`，网关子进程里 `HTTP(S)_PROXY` 还被指到黑洞端口，误连会直接失败而不是把 cookie 发出去。每个 e2e 套件开头还有一条 fail-closed 自检：假上游必须真的收到 `GET /open-apis/user/mi/get`，否则整个套件判失败。

## 命令

```bash
# 先准备二进制（static/ 不入库，必须构建前端）
cd web && npm ci && npm run build && cd ..
go build -o mimo-web2api .

node test/run.mjs --list                  # 列出套件与 tag
node test/run.mjs --tag offline           # 管理接口契约，不需上游
node test/run.mjs --tag e2e               # 全链路，假上游
node test/run.mjs --suite e2e/tools       # 单个套件
node test/run.mjs --tag e2e --concurrency 3

# 浏览器套件（Playwright 自带 Chromium，channel 'chromium' 让扩展在 headless 下也能加载）
cd test && npm install && npx playwright install chromium && cd ..
node test/run.mjs --tag browser
```

二进制默认取仓库根的 `mimo-web2api`，也可以用 `MIMO_BIN=/path/to/bin` 指定。报告写到 `test-results/`（JSON + JUnit XML），CI 会作为 artifact 上传。

## 结构

```
test/
├── run.mjs                 统一入口：选择套件、子进程隔离、超时、汇总、退出码
├── harness/
│   ├── suite.mjs           defineSuite
│   ├── report.mjs          console / JSON / JUnit
│   ├── gateway.mjs         起真网关：复制到临时目录、动态端口、就绪探测、日志环形缓冲
│   ├── fake-upstream.mjs   假上游：7 个端点 + 可编排 SSE 场景 + 请求记录
│   ├── env.mjs             「网关 + 假上游 + API 客户端」一键组装
│   ├── http.mjs            网关客户端（带硬超时）
│   ├── sse.mjs             SSE 编解码
│   ├── selfcheck.mjs       fail-closed：确认上游真的被指到本地
│   ├── browser.mjs         加载真 Chromium + 真扩展，固定扩展 ID、注入 cookie
│   ├── assert.mjs          eventually 等工具
│   └── legacy.mjs          跑 test/sdk 脚本并解析汇总行（不信退出码）
└── suites/
    ├── manifest.mjs        唯一注册表
    ├── index.mjs           按 manifest 动态加载
    ├── meta/registry.mjs   manifest 与磁盘文件双向比对
    ├── contract/accounts.mjs
    ├── browser/extension.mjs  弹窗 → 网关 → 账号池
    └── e2e/*.mjs
```

### 假上游实现的上游端点

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/open-apis/bot/chat`、`/fastchat/open-apis/bot/chat` | SSE：`message` / `usage` / `error`，每帧以空行结尾 |
| POST | `/open-apis/chat/conversation/save`、`/fastchat/...` | 异步保存，用例用 `waitFor` 等它到达 |
| GET | `/open-apis/user/mi/get` | 健康检查 |
| POST | `/open-apis/resource/genUploadInfo` | `uploadUrl` 指回假上游自身 |
| PUT | `/upload/<id>` | 上传二进制 |
| POST | `/open-apis/resource/parse` | 返回资源 `id` |

场景用 `upstream.onChat({ text })` 排队，支持 `{ raw: '<sse 字节>' }`、`{ status: 500 }`、`{ empty: true }`、`{ error: '...' }`。所有请求（含 header、query、body）都记录在 `upstream.requests`，断言口径尽量落在「上游实际收到了什么」，而不是只看网关返回。

## 加一个套件

1. 写 `test/suites/<分类>/<名字>.mjs`，`export default defineSuite({ name: '<分类>/<名字>', tags: ['e2e'], async run(t) { ... } })`
2. 在 `test/suites/manifest.mjs` 里加一行
3. `meta/registry` 会校验两边一致，漏了会红

用例用 `t.test('名字', async () => { ... })`：单个用例失败不会中断同套件其余用例。

## 注意

- 套件之间可以并发（各自独立网关进程、独立临时目录、独立端口）；套件内部必须串行 —— 网关的会话缓存、媒体缓存、账号轮转都是进程级状态。
- 网关的 `data/` 目录跟着可执行文件走且不可配置，所以 `gateway.mjs` 每次都把二进制复制进自己的临时目录，避免 `stats.json` / `conversations.json` 互相污染。
- 真实上游套件不要往 CI 里接：它们需要真实账号，且一次全跑约 25 分钟。`legacy.mjs` 只是给本地用得顺一点。
- 浏览器套件用 Playwright 自带的 Chromium，不是 branded Chrome —— Chrome 137 起移除了 `--load-extension`，只有 Chromium 还能 side-load 未打包扩展。
- `browser-extension/manifest.json` 里的 `key` 是公开公钥，只为把扩展 ID 钉死（`chrome-extension://<id>/popup.html` 才能被测试直接访问）；私钥不落仓库，unpacked 加载不需要它。
