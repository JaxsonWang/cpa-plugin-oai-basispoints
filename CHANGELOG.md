# 更新日志

## v0.1.12 — 2026-09-25（UTC+8）

- 仅修改插件：声明 CPA 已有的 `codex` 输入/输出格式，复用宿主 Claude↔Codex 转换；不新增私有转换器，不修改 CPA 主程序。
- 修正转换请求误读 `OriginalRequest` 的问题，使用宿主规范化后的 `Payload` 进行上游请求、工具目录校验及历史回放。原生 Responses 路径保持原文处理。
- 按宿主契约输出 Codex 非流式终态事件与逐条流式 data 事件，保留工具参数、完成/截断状态和用量。移除此前主动拒绝 Claude 的认证前拦截。
- 移除独立 token 计数返回 0 的占位结果，改为明确的 `unsupported_token_count`，不伪造计数成功。
- 独立 `/responses/compact`、`previous_response_id` 续接及 Fast/priority 仍未实现；之前修复的是错误处理及静默丢失风险，不是这些能力本身。上游流式仍全量缓冲后回放。
- 包含此前未单独发布的 v0.1.11 修复：支持 Codex CLI 的 `input[].additional_tools`，统一工具目录、调用校验及历史回放，解决已复现的 `tool_not_in_catalog`。
- 已完成原版 CPA v7.3.17 的 54 项隔离宿主检查，以及真实生产 Basis Points 验收；不需要修改 CPA 源码。
- 生产验测：Claude Code 2.1.247 文本及 `Read → Write → Bash` 四轮工具调用通过；Claude HTTP 非流式/流式、工具回传、完整历史多轮以及 Codex CLI 0.156.1 工具回归通过。23 次记录的生产 API 请求均返回 200。
- 验证边界：Codex 文本首测多输出句号，严格比对未通过；保留该记录，使用无歧义提示后的单次复测精确通过。上述验收不等于长时间压力、大上下文或全部多模态场景验证。

## v0.1.11 — 2026-09-25（UTC+8）

- 修复 Codex CLI 将工具定义放在 `input[].additional_tools` 时出现的 `tool_not_in_catalog`：合并顶层及输入内工具声明，并统一目录、调用校验与历史回放。
- 保留 function/custom 工具命名空间及 `tool_choice` 约束；同名工具使用后续声明，避免目录与校验规则不一致。
- 转换后的上游请求移除原始 `additional_tools`，仅通过已有中继协议传递客户端工具。
- 本次未新增 Claude/Anthropic Messages、Fast、独立压缩或 ID 续接能力；部署及真实客户端验收结果另附。

## v0.1.10 — 2026-09-25（UTC+8）

- #3：移除 Fast 能力声明；普通档位不发送 `service_tier`，显式优先档位明确拒绝。
- #6：增加不包含私有参数的工具格式诊断及最多一次重生成；最终格式错误以 422 返回，在流式开始前保留状态，避免 CPA 将其误判为凭据冷却。
- #7：通过认证前请求拦截返回明确的 Claude 协议不支持错误，保留原生模型路由；未新增 Anthropic 转换能力。
- #8：非流式按上游正文格式解析 JSON/SSE，保留终态和用量、修正响应实体头，并拒绝空正文、HTML、失败或不完整事件序列；线上原始故障仍待新包部署验收。
- #9 / #10：明确拒绝未验证支持的独立压缩及 ID 续接，避免普通生成冒充压缩或上下文静默丢失。
- 补充请求边界、HTTP 故障注入、工具往返及错误后凭据可用性验证。发布包覆盖 Linux/macOS/Windows 的 AMD64/ARM64。

### 升级注意事项

- 替换插件后重启 CPA，并在插件管理中确认已加载 `0.1.10`；发布 Release 不会自动替换正在运行的动态库。
- Fast、Claude/Anthropic Messages、独立 `/responses/compact` 与 `previous_response_id` 续接仍不支持。本版改为明确拒绝，不能将错误处理修复理解为新增这些能力。
- #8 的 JSON/SSE 处理已通过组件及隔离宿主验证，但原线上失败请求的 Basis Points 原始正文尚未捕获，新包真实上游和 Linux 实机仍待验收，不宣称线上故障已根治。
- 已有 CPA 集成验证实际加载的是同源码 macOS 动态库，上游为明确标注的合成 HTTP 夹具；真实 Claude Code CLI 验证的是明确 400 拒绝，不是成功推理。
- 上游 SSE 仍全量缓冲后回放；工具格式错误最多增加一次重生成，可能增加上游用量。

## v0.1.9 — 2026-09-24（UTC）

### 新增与改进

- 新增 `model_mappings`，支持任意数量的客户端别名分别映射到不同上游模型，可在同一插件实例中同时配置 Astra、Sol 及其他实际可用模型。
- 模型注册、普通请求、流式请求及 Codex 模型目录元数据使用同一份映射；上下文容量按各自规范模型读取，不复用其他模型的数值。
- 保留原有单上游配置：未单独映射的别名继续使用 `upstream_model`。校验未知别名、空映射和去除首尾空白后的重复映射键，避免误路由。
- 补充配置重载、持久化、并发隔离和 1、2、3、25、100 个模型的回归测试。
- 将 `config.example.yaml` 改为完整 CPA 宿主配置，并补全中文注释、模型增删方法及配置优先级说明；修正插件元数据中的仓库地址。

### 升级注意事项

- 配置位于 `plugins.configs.oai-basispoints`；`models` 声明启用的别名，`model_mappings` 声明对应上游，增加或删除模型时同步修改两处。
- 推荐按示例显式设置 `data_dir: ""`，仅使用 YAML 配置。若保留持久化目录，旧 `settings.json` 中的字段仍优先于 YAML，可能导致新增模型不显示或出现 `model_mappings alias is not enabled in models`。本版未改变持久化配置的优先级。
- 替换实际生效的插件动态库后重启 CPA，再刷新模型列表；不要通过改插件 ID 或另存同目录副本来替代原插件。
- 发布检查不等于真实上游联调；模型可用性仍受 Basis Points 和账号权限限制。Fast、流式缓冲及 OAuth 刷新同步等既有限制保持不变。

## v0.1.8 — 2026-09-25

本版统一发布此前工作区中的图片、上下文和工具协议修复，并新增 CPA 第三方插件源与多平台 GitHub Actions 打包。

### 修复与改进

- 用户消息中的内嵌图片先上传到 Basis Points 附件接口，再发送真实 `file_id`；保留图片字节和明确精度，并隔离账号/令牌缓存。
- 修正非流式宿主 HTTP 回调的 `StatusCode / Headers / Body` 字段匹配，避免上传成功却被误报 `HTTP 0`。
- 未指定、`null` 或空的 `context_management` 不再发送；保留显式非空策略，不注入隐式 200k 阈值。
- 使用 CPA v7.3.16 已有模型目录响应钩子修正本插件别名的上下文元数据，不要求修改 CPA 主程序。
- 修复客户端 function/custom 工具的命名空间、完整参数目录、结果与原生调用回放，以及 SSE 工具参数事件；保留多模态工具结果、大整数和空白。
- 新增根目录 `registry.json`，通过 `plugins.store-sources` 接入 CPA 插件商店；版本由 GitHub 最新 Release 决定。
- GitHub Actions 校验并构建 Linux/macOS/Windows 的 AMD64/ARM64 动态库，发布平台压缩包及 SHA-256 校验清单。

### 已知问题与边界

- **Fast 尚未修复，继续在 [#3](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/issues/3) 跟踪。** 真实对照中，不传 `service_tier` 成功；传 `default` 或 `priority` 均返回 422。请省略该字段，不能将 Fast 入口或参数透传当作优先调度已生效。
- [#2](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/issues/2) 按维护者实际使用反馈关闭，不代表全部线上场景、500k 实际容量或 Fast 已验收。
- 上游 SSE 仍全量缓冲后回放；token 计数及跨虚拟认证的 OAuth 刷新同步限制保持不变。
- 本地测试、Actions 打包及插件商店安装契约校验，不等于已自动更新用户的 CPA 部署。更换已加载动态库后请重启 CPA。
