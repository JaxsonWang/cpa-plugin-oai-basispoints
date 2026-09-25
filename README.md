# CPA OpenAI Basis Points 插件

这是一个 CLIProxyAPI（CPA）原生插件，用 CPA 已有的 ChatGPT/Codex OAuth 凭据直接请求。

## 通过 CPA 插件商店安装（推荐）

在管理界面的「第三方插件源 → 插件源 registry URL (plugins.store-sources)」中添加以下地址并保存，然后刷新插件商店，搜索 **CPA OpenAI Basis Points**：

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

插件的 `auth.parse` 会接管 CPA 中 `type: codex` 的 OAuth 文件，并为同一个文件展开两条内存认证：一条保留原生 `codex`，另一条是 `oai-basispoints` 虚拟认证。这样现有 Codex 模型继续使用 CPA 原生执行器，`gpt-6-astra-basispoints` 则使用本插件；不会生成或改写 OAuth 文件。原生 Codex 记录保留源 OAuth 元数据，供原生执行器读取访问令牌和刷新令牌。注意：当前 CPA 会把这两条记录都标记为虚拟认证，不持久化原生记录的刷新结果；Basis Points 记录也不会自动同步原生记录在内存中刷新的 JWT。源 JWT 过期时，需要先通过 CPA 更新或重新导入源 OAuth 凭据，再重新加载，单纯重载过期文件无效。流式响应遵循 Responses SSE 格式，但为保证工具调用可在完整 item 上做安全转换，当前会先读完上游 SSE 再回放给客户端，不是 token 级实时转发。

## 构建

```bash
make test
make build
```

## v0.1.10 更新与边界

本版针对 issues #3、#6、#7、#8、#9、#10 修正协议边界与错误处理。通过插件商店更新或手动替换动态库后，需要重启 CPA 才会加载新代码。

- **普通与 Fast 档位**：未指定、`null`、`auto`、`default` 使用普通模式，不向 Basis Points 发送其拒绝的 `service_tier` 字段。`priority` / `fast` / `flex` 等其他值返回 400；模型目录不再声明 Fast，不能理解为已支持加速。
- **工具格式错误**：仅在输出交付前，针对无效中转载荷最多重新生成一次；不猜测修补 JSON、不执行畸形调用、不交付半条成功结果。仍失败时返回 `422 invalid_tool_call`，附原因类别和可用的 JSON 字节偏移，不包含参数正文。当前 CPA JSON ABI 没有请求级错误标志，因此使用其不会冷却凭据的 422；此错误来自模型输出，不表示 OAuth 失效。重生成可能增加一次上游用量，返回的 `usage` 仍为最终 Response 的原始用量。
- **Claude Code / Anthropic Messages**：仍未实现完整协议转换。对本插件模型的 `/v1/messages` 请求在认证选择前返回明确 400；不再落入宿主的协议选择 500 和后续凭据冷却。不影响原生 Codex 模型的路由，也不虚报 `claude` 格式支持。
- **非流式**：按上游实际正文解析 JSON 或 SSE，返回完整 Response JSON，保留输出、状态与用量；HTML、空正文、流错误和缺失终态仍明确失败，并提供安全的响应类型/长度诊断。没有捕获原线上失败请求的 Basis Points 原始正文，因此不能将此项组件修复等同于线上 #8 已根治。
- **独立压缩与 ID 续接**：`/responses/compact` 及非 `null` 的 `previous_response_id` 返回明确 400。继续使用 `/responses` 并回传完整消息、工具调用及结果历史；不会把普通回答冒充压缩结果，也不会静默丢弃续接 ID。这不是新增独立压缩或 ID 续接能力。

流式路径仍全量缓冲上游结果，然后回放合法 SSE；终态校验和最多一次重生成在返回客户端响应前完成，保留 HTTP 错误状态。不会把 `incomplete` 改成 `completed`。

本地验收区分：Go 回归、原版 CPA 加载同源码 macOS 动态库的真实 HTTP/CLI 链路测试（上游为明确标注的合成夹具）、现有线上服务对照。Linux `.so` 的目标系统加载及真实 Basis Points 推理仍需独立验收；发布 Release 不代表已经更新用户的 CPA 部署。

## 协议边界

- 上游请求始终带 `Authorization: Bearer <access_token>`、`chatgpt-account-id`、`x-openai-account-id` 和 `x-basispoints-auth-mode: chatgpt`。
- `turn_id` 按会话和当前用户 turn 稳定生成；工具结果回合只递增 `agent_iteration`，不会把同一 turn 重新当成新计划。
- 工具 `code` 是嵌套 JSON 字符串，不是 JavaScript。插件只解析它，不执行其中内容。
- 未能从 OAuth JWT 或凭据字段得到账号 ID、token 过期、上游返回非 2xx、工具名不在客户端目录中时，插件会报告明确错误，不伪造成功。

---

## 版权与社区支持

本项目基于 [MIT License](LICENSE) 开源

感谢 [LINUX DO 社区](https://linux.do/) 的支持

<a href="https://linux.do/">
  <img src="docs/assets/linuxdo.png" alt="LINUX DO 社区" width="360" />
</a>
