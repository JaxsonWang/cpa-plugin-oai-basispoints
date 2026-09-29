# 纯插件修复候选线上验测

日期：2026-09-29（UTC+8）。用户确认已替换 Linux AMD64 `.so` 后执行。没有修改 CPA 源码、认证、服务端配置或客户端全局配置，没有重启服务。

## 加载确认

- Chrome 管理页面显示 `OpenAI Basis Points` 版本 `0.2.6`，状态为生效中、已注册、已配置，加载路径为 `plugins/linux/amd64/oai-basispoints.so`。
- 运行日志已经出现本次新增的结构化错误诊断格式，能够保留 `rate_limit_exceeded` / `rate_limit_error` 和 429 分类，证明新错误处理逻辑已执行。
- 未独立读取服务器动态库 SHA256，因此不把页面版本号当成二进制哈希核验。

## 真实接口结果

模型：`gpt-6-astra-basispoints`。使用随机标识和合成工具结果，串行执行，推理强度 `low`；未设置生产重试参数，没有故意制造限流，也未上传原会话或真实业务数据。

| 检查 | 结果 | 耗时 |
| --- | --- | --- |
| 模型目录 | HTTP 200，配置模型存在 | 0.132 秒 |
| 不支持的结构化格式 | 明确 HTTP 400，符合既有插件边界 | 0.024 秒 |
| HTTP 非流式文本 | completed，随机标识完全正确 | 5.271 秒 |
| HTTP SSE 文本 | completed，6 个正文增量与最终内容完全一致 | 5.041 秒 |
| function 工具调用 | completed，`fixture_echo` 名称、调用数量和参数正确 | 4.574 秒 |
| 工具结果回传及流式回复 | completed，准确返回合成工具结果 | 4.077 秒 |
| WebSocket 第一轮 | completed，正文增量与终态一致 | 16.095 秒 |
| WebSocket 同连接增量续接 | 使用真实上一轮 `previous_response_id`，准确保留上下文，completed | 16.804 秒 |

共 8 项检查通过，其中验测器发出的 6 次生成请求均取得成功终态。HTTP SSE 额外校验事件序号、消息/片段生命周期、正文去重、唯一成功终态及错误事件。工具是无外部副作用的合成 echo，不代表执行了真实 shell 或业务写操作。验测器没有自动重试，但 CPA 的既有内部重试配置未改变，因此不据此断言上游只执行了六次尝试。

## 新日志揭示的限流

运行日志中观察到以下样本。本轮没有故意触发限流或进行压力测试；前两条早于接口验测，其余记录未逐一关联到具体会话，不用于推断调用来源：

| 北京时间 | 请求 ID | 分类 |
| --- | --- | --- |
| 17:12:48 | `cb8d731c` | 429 / rate_limit_exceeded / rate_limit_error |
| 17:13:13 | `cb8d731c` | 429 / rate_limit_exceeded / rate_limit_error |
| 17:19:20 | `68d9f0cb` | 429 / rate_limit_exceeded / rate_limit_error |
| 17:20:29 | `36699035` | 429 / rate_limit_exceeded / rate_limit_error |

日志不再将这些失败统一记录为 `502 Basis Points stream reported a failure`。错误事件来自 Basis Points 上游；429 状态和错误类型可能由插件依据上游错误码映射，不一定是上游原始 HTTP 状态码。本次修复改善了真实错误分类与诊断，不会取消上游限流。不能仅凭该错误码区分请求频率、token 速率或并发限制，也不能把升级前全部缺少原始信息的 502 都追溯认定为限流。

未修改的 CPA 仍可能将限流等非请求类错误表现为断开连接。因此，“验测通过”不表示所有 `Connection reset without closing handshake` 都已消失。

## 证据与边界

- 证据目录：`build/live-plugin-only-20260929/171633/`。
- 汇总：`acceptance-summary.json`；HTTP：`summary.json` 及逐项结果；WebSocket：`websocket-results.jsonl`；脱敏日志摘录：`observed-server-errors.json`。
- 插件源文件与已打包候选清单逐文件 SHA256 一致；CPA 工作区保持干净，客户端配置哈希未改变。
- 未重放最初的长上下文会话，没有压力测试，没有覆盖 Windows GUI、所有历史图片组合或所有工具类型。
- 当前结论：新插件已运行，所测核心链路通过；自然流量仍有上游限流，须与插件错误分类修复区分。
