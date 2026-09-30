# CPA OpenAI Basis Points 插件

这是一个 CLIProxyAPI（CPA）原生插件，用 CPA 已有的 ChatGPT/Codex OAuth 凭据直接请求。

## 通过 CPA 插件商店安装（推荐）

在配置面板→高级与实验→第三方插件源 → 插件源 registry URL (plugins.store-sources)」中添加以下地址并保存，然后刷新插件商店，搜索 **CPA OpenAI Basis Points**：

```text
https://raw.githubusercontent.com/JaxsonWang/cpa-plugin-oai-basispoints/main/registry.json
```

也可合并到 CPA **宿主配置**（`config.yaml`，与下方插件配置共用同一个 `plugins` 节点）：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/JaxsonWang/cpa-plugin-oai-basispoints/main/registry.json
```

保留已有插件源，不要整体覆盖原有 `plugins` 配置；内置官方源由 CPA 自动保留。本源使用宿主原生的 `github-release` 安装方式，最新版本以本仓库已发布的 GitHub Release 为准，不在 registry 中另行维护版本号。CPA 会按运行平台下载 `oai-basispoints_<version>_<goos>_<goarch>.zip`，并使用同一 Release 的 `checksums.txt` 校验。

发行包覆盖 Linux、macOS、Windows 的 AMD64/ARM64。插件商店负责下载、校验和安装；更新已加载的动态库后仍需重启 CPA，使新代码及 OAuth 认证解析生效。

## 安装和配置

1. 将 `build/linux/amd64/oai-basispoints.so` 复制到 CPA 的 Linux amd64 插件目录。
2. 将 `config.example.yaml` 按需合并到 CPA 的 `config.yaml`；它是完整的宿主配置示例，不会由插件自动读取。插件内置默认暴露 `gpt-6-astra-basispoints`，示例同时配置 Astra 和 Sol，可继续增删模型。
3. CPA 的 `auth-dir` 中已有的 `type: codex` OAuth 文件会被插件识别；插件只在内存中读取 token，不生成另一份 token 文件。
4. 客户端使用 Responses 协议调用 `gpt-6-astra-basispoints`。模型目录声明图像输入，以及 `low`、`medium`、`high`、`xhigh`、`max`、`ultra` 思考等级；`max` 映射为 `xhigh`，`ultra` 原样传递，未指定时默认 `medium`。

默认使用 host 模式：Codex OAuth 文件由 CPA 原生加载、刷新和持久化；插件不接管认证解析，只在执行 Basis Points 别名模型时读取当前凭据。现有 Codex 模型继续使用 CPA 原生执行器，插件不生成或改写凭据文件。

## 凭据来源与刷新持久化

`credential_source` 未配置或留空时默认为 `host`，让 CPA 长期自动维护同一份 OAuth 凭据：

- 插件不再接管 `type: codex` 文件解析，CPA 按原生 Codex 认证加载、刷新，并把轮换后的 `refresh_token` 写回源文件；重启后读取的是最新凭据。
- 插件改为声明模型路由器，只把配置中的别名模型交给本执行器；原生 Codex 模型的调度、冷却与重试不受影响。
- 每次执行通过 `host.auth.list` / `host.auth.get` 读取宿主当前凭据，轮询未禁用、非运行时且 token 未过期的 Codex 认证。不缓存 token；过期文件不挤占有效账号的轮询份额。没有候选时返回 503 `auth_unavailable`；全部读取或校验失败时，按文件名稳定顺序返回实际失败原因，例如 401 `auth_expired`，不会随轮询位置随机改变错误分类。
- 不共用原生 Codex 通道的过载冷却，也不在一次已发送的生成失败后自动换号重放；401/403/407/429 保留真实状态。此模式不经过 CPA 原生认证调度器，原生账号级冷却、优先级及用量归属不能视为已自动继承。
- `model.static` 提供宿主全局代理；凭据自身 `proxy_url` 优先，其次为宿主 `proxy-url`，都未配置才使用环境代理。`direct` / `none` 明确禁用代理。HTTP、SSE、图片上传和 WS 使用同一份有效代理策略；错误或不可达的代理不允许绕过直连。
- 未修改的 CPA `host.http.*` ABI 不能给 self 路由重新绑定账号代理，因此 host 模式的 HTTP/SSE/附件由插件绑定本次凭据的 HTTP 客户端发送，并保留取消、超时、停用及大小限制；不是失败后的备用通道。连接池按账号与有效代理隔离，不缓存 OAuth token。该路径不经过宿主 `host.http.*` 请求日志捕获。

仍可显式设置 `credential_source: virtual`：`auth.parse` 接管 Codex 文件并展开原生 Codex 与 Basis Points 两条内存认证。当前 CPA 会把两条记录都标记为 `plugin_virtual`，不持久化原生刷新结果，Basis Points 记录也不自动同步刷新的 JWT；因此不建议用于长期运行。已有 YAML 或 `data_dir/settings.json` 中显式保存的 `virtual` 不会被新默认值覆盖，需改为 `host` 或删除该字段；已有失效凭据仍需通过 CPA 更新或重新导入，新默认值不会修复已轮换失效的 token。

插件不再提供独立设置页面或源认证编辑接口。请直接在 **CPA 凭证设置**中管理 `websockets` 等字段；`upstream_transport: auto` 读取当前凭据的开关，`http` 则始终禁用 WS。host 模式下一次请求读取已保存的新开关，无需维护第二份设置。

## 构建

```bash
make test
make build
```

## 协议边界

- Codex 模型目录为本插件别名同步对应规范模型的 `apply_patch_tool_type`，保留缺失与空值语义。更新后需让 Codex 刷新模型目录，再用新会话验测原生补丁及差异入口；不回填历史会话事件。
- 客户端实验工具仅按对应规范模型开放已接入的时钟和异步提问；不声明尚未实现密文消息契约的多代理 v2。旧缓存或显式启用 v2 的请求若包含代理密文会明确返回 400，不会强转明文。不会批量复制 Code Mode、审核策略或未知实验工具，也不会改写客户端个人配置。
- 明文 `agent_message` 保留原生 `author` / `recipient`、消息类型及正文；上游要求 `author`，不能删除后用正文标记代替，也不为缺失字段伪造身份。此行为不代表已支持多代理密文协议。
- 结构化 `text.format`（`json_object` / `json_schema`）尚未实现，显式返回 400；不丢弃 Schema 后返回普通正文冒充成功。`text.verbosity` 的上游映射仍未实现，本版保留既有行为以避免默认请求回归，不宣称详细程度参数已生效。

- 上游请求始终带 `Authorization: Bearer <access_token>`、`chatgpt-account-id`、`x-openai-account-id` 和 `x-basispoints-auth-mode: chatgpt`。
- `turn_id` 按会话和当前用户 turn 稳定生成；工具结果回合只递增 `agent_iteration`，不会把同一 turn 重新当成新计划。
- 工具中继通过外层 `references: [完整工具名]` 路由，`code` 只承载该工具的载荷：function 工具为参数 JSON 对象，custom 工具为逐字保留的原始文本。不要再套 `{tool,args}` 内层包装；插件不执行其中代码。已有会话的原生历史调用原样回放，新调用按本次注入的协议生成。
- 默认 `stream_tool_mode: incremental` 从首条非空正文或推理摘要增量开始交付，实时保留推理条目及 `response.reasoning_summary_*` 生命周期；终态补齐尚未交付的摘要和正文，不重复已有增量。上游完全不发送摘要或正文时，仍需等待有效内容，不伪造心跳或摘要。
- 对 #21 的“正文/摘要先行、随后工具 JSON 失败”，可在插件配置中显式设置 `stream_tool_mode: buffered`：本轮有可调用工具时等待整轮校验，失败尝试的正文、摘要和工具均不交付，再使用原有至多一次重生成。仅通过校验的最终响应交付一次；两次均失败仍返回真实 422，不自动修补 JSON、不伪造局部工具成功。代价是所有可调用工具回合（即使最终只有正文）都要等待整轮完成及可能的一次重生成，不再实时输出首字/摘要；无工具或 `tool_choice: none` 不受影响。HTTP/SSE 与上游 WS 共用此策略，上游仍使用流式，大小和超时限制不变。若 `data_dir` 非空，已有 `settings.json` 对应字段仍会覆盖 YAML。
- 非法函数 JSON、目录外工具或不符合 schema 的参数仍严格拒绝，不猜测修补引号、不丢弃坏调用，也不交付半批工具。非流式或尚未交付正文/推理摘要的请求保留最多一次重生成，最终工具格式失败返回 422。
- 已交付正文或推理摘要后发生工具校验失败、上游失败或截断时，保留已交付内容并发送明确的协议 `error`，不再重跑该请求，不发送成功终态；已提交的 HTTP 200 无法改为 422/502。客户端需检查流内事件，而不只检查 HTTP 状态。客户端自行重连或另发非流式请求不受插件控制。
- 上游 `error`、`response.failed`、`response.cancelled` 在 JSON、SSE、WebSocket 路径统一分类：优先保留明确的错误状态，否则依据已知 `code` / `type` 分类；未知错误仍按上游失败处理。流内错误携带 `status`，跨插件 ABI 保留结构化分类，避免参数错误和限流都变成普通 502。
- 失败诊断只保留已知的协议分类及事件名称，不回显上游自由文本、请求正文、图片、任意字段或凭据。错误分类和透传优化仅在插件内实现，不要求修改或替换 CPA 主程序。CPA 的下游 WebSocket Close 帧由宿主负责，不能把插件分类通过等同于宿主关闭握手已修复；纯插件验证及边界见 [断流错误处理验测](docs/validation/stream-failure-close.md)。
- 保留文本空白、消息和内容索引、最终用量及完成/截断状态；断开连接时关闭上游，插件停用时取消并等待活动流退出。该路径不改变原生工具身份回放、补丁内容及 Codex/Claude 格式转换规则。
- 未能从 OAuth JWT 或凭据字段得到账号 ID、token 过期、上游返回非 2xx、工具名不在客户端目录中时，插件会报告明确错误，不伪造成功。

---

## 版权与社区支持

本项目基于 [MIT License](LICENSE) 开源

感谢 [LINUX DO 社区](https://linux.do/) 的支持

<a href="https://linux.do/">
  <img src="docs/assets/linuxdo.png" alt="LINUX DO 社区" width="360" />
</a>
