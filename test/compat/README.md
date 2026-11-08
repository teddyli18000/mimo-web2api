# 兼容性测试环境（fake → real）

同一套 `cases.mjs`，两种上游。原则：**先在本地把 fake 跑绿，再打真机**；CI 只跑 fake。

## 凭据处理

- fake 模式不需要任何凭据（用的是 dummy 账号 + 本地假上游）。
- real 模式只接受配置**路径**：`--config <path>` 或环境变量 `MIMO_REAL_CONFIG`。
  配置内容只被复制到系统临时目录，进程结束立即删除；永远不写进项目、不进日志、不进报告。
- 所有报错都经过 `redact()`，把 `api_key` / `service_token` / `user_id` / `ph` 换成 `***`。
- `fixtures/` 只是脱敏后的工具 schema 和系统提示词，不含任何凭据。

## 跑法

```bash
# 1) 假上游（CI 也是这一套，先过这关）
node test/compat/run.mjs --mode fake --bin dist/mimo-web2api.exe

# 2) 真机（fake 全绿之后）
MIMO_REAL_CONFIG=C:\\path\\to\\config.json \
  node test/compat/run.mjs --mode real --bin dist/mimo-web2api.exe
```

Windows 也可以把 `MIMO_REAL_CONFIG` 设成环境变量，`--bin` 省略时用 `MIMO_BIN` 或仓库根默认二进制名。

CI 侧对应 `e2e/compat-dsh`（`node test/run.mjs --tag e2e` 自动带上），用的是 `test/harness/fake-upstream.mjs`。

## 覆盖什么

- DSH 形状：50 个真实工具 + 长系统提示词
- 列目录（`glob`）→ 读文件（`read`）→ 工具结果回传 → 继续
- **工具名必须来自声明列表**、**必填参数必须齐全**（真机实测过模型只吐空 `<invoke>` 的情况）
- 超预算系统提示词下，调用契约不被截断、query 不超 90K
- 无前缀 `/models` 与 `/v1/models` 一致
- Responses 路径的 `function_call` 项与 `call_id`

fake 模式额外能抓上游 query，所以能精确断言「契约进了 prompt / 续轮没重发历史 / 纠正提示生效」；real 模式只断言返回结构合法（真实模型的措辞不可控，不做精确文本断言）。
