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

插件的 `auth.parse` 会接管 CPA 中 `type: codex` 的 OAuth 文件，并为同一个文件展开两条内存认证：一条保留原生 `codex`，另一条是 `oai-basispoints` 虚拟认证。这样现有 Codex 模型继续使用 CPA 原生执行器，`gpt-6-astra-basispoints` 则使用本插件；解析和执行请求不会生成或改写 OAuth 文件，只有用户通过下述源认证入口保存时才修改原文件的 `websockets` 字段。原生 Codex 记录保留源 OAuth 元数据，供原生执行器读取访问令牌和刷新令牌。注意：当前 CPA 会把这两条记录都标记为虚拟认证，不持久化原生记录的刷新结果；Basis Points 记录也不会自动同步原生记录在内存中刷新的 JWT。源 JWT 过期时，需要先通过 CPA 更新或重新导入源 OAuth 凭据，再重新加载，单纯重载过期文件无效。

## 凭据来源与刷新持久化

默认 `credential_source: virtual` 保持上述展开方式。需要让 CPA 长期自动维护同一份 OAuth 凭据时，设置 `credential_source: host`：

- 插件不再接管 `type: codex` 文件解析，CPA 按原生 Codex 认证加载、刷新，并把轮换后的 `refresh_token` 写回源文件；重启后读取的是最新凭据。
- 插件改为声明模型路由器，只把配置中的别名模型交给本执行器；原生 Codex 模型的调度、冷却与重试不受影响。
- 每次执行通过 `host.auth.list` / `host.auth.get` 读取宿主当前凭据，按稳定顺序轮询未禁用、非运行时的 Codex 认证；access_token 已过期的凭据跳过，均不可用时返回 503 `auth_unavailable`。原生通道的过载冷却不作为跳过条件。
- 此模式不声明认证提供方，CPA 不再向插件提供认证目录，源认证 WS 编辑页面不可用；WS 开关仍读取源文件的 `websockets` 字段，可直接编辑源文件或使用 CPA 管理接口修改。

## 构建

```bash
make test
node --test internal/basispoints/source_auth_page.test.mjs
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
- 流式请求从首条非空正文或推理摘要增量开始交付，实时保留推理条目及 `response.reasoning_summary_*` 生命周期；终态补齐尚未交付的摘要和正文，不重复已有增量。上游完全不发送摘要或正文时，仍需等待有效内容，不伪造心跳或摘要。
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
