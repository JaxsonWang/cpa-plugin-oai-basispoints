# PR #13：安全收窄与回归验证

2026-09-27（UTC+8），在独立 worktree 处理 #13。基线为已经合并 #14 的 `e994d4c8ab6ad98ed98d9695546bf361b4cccc0f`；原 #13 head 为 `d94fd04981a3638a504028248ad3028e392dda87`。未修改另一个会话所在 main 工作区，未将尚未提交的 WS 改动纳入本次验测。

## 最终范围

- 仅保留 `tool_choice` 诊断改进：目录中存在但本轮不允许的工具返回 `tool_not_allowed_by_tool_choice`，真正未声明的工具仍返回 `tool_not_in_catalog`。
- 两类失败仍为 `422 invalid_tool_call`；未授权工具不交付、不缓存，错误消息不包含工具正文。
- 撤回 JSON 自动修复层，保持完整对象、严格解析、尾随内容拒绝及既有有界重生成。custom 工具原文仍不按 JSON 解析。
- 撤回按类型静默删除历史条目的黑名单，保留既有请求处理行为。没有声称 Basis Points 支持全部原生工具历史，也没有宣称修复所有 `Invalid request body`。
- 合入当前 main 后，新增测试使用 #14 的单参数 `translateInputItems`。#14 的 custom ID 和无目录历史回放修复保留。
- 不新增配置、重试机制、降级路径、WS 支持或发布版本。

## 先失败再通过

保留原 #13 行为的合并副本上，三个历史结果保留用例、九个非法 JSON 拒绝用例先失败。撤回不安全逻辑后全部通过。

原审查报告中的七个子用例另以 Go overlay 原样复验：围栏内外的尾随内容、缺失数组元素/对象成员、MCP/shell/computer 历史结果，全部通过。该 overlay 只用于本地复核；对应边界已纳入正式提交的测试。

新增测试还覆盖：

- function/custom、平面/命名空间、顶层/`additional_tools` 目录，共 56 个诊断与授权组合。
- `none`、强制其他工具、`allowed_tools` 排除当前工具的准确拒绝，以及被允许工具正常交付。
- 非法 JSON 整批拒绝、无部分缓存、无输入修改、无私有内容泄漏。
- custom 原文中的围栏、非法 JSON 样式、换行与空白保持不变。
- 已有消息、纯图片、reasoning 密文、compaction trigger 和未知条目保持原行为。

## 完成的验证

环境：Go `1.26.2`、macOS arm64。

| 检查 | 结果 |
| --- | --- |
| `go test -race -count=1 -json ./...` | 509 项测试/子测试通过，零失败 |
| 原七个边界子用例 | 全部通过；连同三个父测试共十项通过 |
| 相关回归 `-race -shuffle=on -count=5` | 通过 |
| `go vet ./...`、`go build ./...` | 通过 |
| `go mod tidy -diff`、gofmt、actionlint `v1.7.12` | 通过 |
| 原版 CPA v7.3.17 + 候选 macOS 动态库 | 96 项 HTTP/CLI 回归通过 |

宿主回归沿用 v0.1.18 的已验证夹具，覆盖 Responses/Claude 流式与非流式、工具原文与历史往返、usage、一次重生成/最终 422、失败后凭据可用性、增量正文及已开流后的错误边界。实际 Claude Code 2.1.247 完成只读 Bash 往返；Codex/Claude 对坏工具和截断明确报告失败。

**真实的是 CPA、候选动态库和客户端；推理上游是本机合成 HTTP 服务，不是生产 Basis Points。** 本次未部署、未执行真实上游验收，也未验证 Linux 实机或其他会话尚未提交的 WS 业务。

本地验测曾因 overlay 夹具被保存为 build 目录下的独立 `.go` 测试文件而造成额外测试包编译失败。已将夹具改为非 Go 扩展名，并重跑完整检查；没有排除正式源码或降低测试门槛。该失败日志保留在独立工作区的验测目录。

候选 macOS 动态库 SHA-256：`a29fd9a3b4edbfc3ebcd1dab9109e7c76632a78cf5ce5fe3654e030d18202367`。这是本地验测包，不是发布资产。

原始日志保存在独立工作区的 `build/pr13-validation/`：`before-repair.jsonl`、`tests.jsonl`、`original-review.jsonl`、`checks.json`、`shuffle.log`、`host/host-integration-results.json`。GitHub CI 状态以最终 PR head 的实际检查结果为准。

## 合入 WS 主分支后的整体验测

同日用户要求将 #13 与已完成的 WS 开发合并并更新 SO。在独立工作区合入主分支 `5844695dcdbbe312301cda13bf2bea409415d9fe`；保留其凭据级 WS 门槛、代理继承、单次握手回退与已提交请求不重放规则。

首次完整回归 627 项通过，但随机顺序复测暴露 WS 生成超时偶发返回 499 的竞态。根因是 `runningStream.contextError` 两次读取 `ctx.Err()`：第一次仍为 nil、第二次变为 `DeadlineExceeded` 时，进入了客户端取消分支。新增受控状态转换回归先失败，再改为基于单个状态快照分类；不新增重试或回退，也不放宽错误断言。

| 最终合并候选检查 | 结果 |
| --- | --- |
| 完整 `go test -race -count=1 -json ./...` | 628 项测试/子测试通过，零失败 |
| 原失败随机种子、相关用例五轮 | 通过 |
| WS 生成超时与新竞态回归二十轮 | 通过 |
| vet、build、模块一致性、gofmt、actionlint | 通过 |
| 原版 CPA WS 回归 | 107 项通过；97 次握手经过认证 CONNECT 代理、HTTP 回退 POST 为零 |
| 原版 CPA 强制 HTTP 回归 | 96 项通过 |
| 凭据开关门禁 | missing/false/true 三个独立实例，流式/非流式共 6 项通过；源凭据不修改 |
| 原生 Codex 补丁 | 实际文件增删改、fileChange 与差异事件、工具结果往返通过 |
| 原生客户端能力 | 时钟、休眠、异步答案返回上下文通过 |

上述宿主检查均使用此次最终源码重新构建的 macOS 动态库和本地合成推理上游，不复用旧版本二进制的通过结果。所有记录及首次失败日志保存于独立工作区 `build/pr13-ws-integration/`。Linux SO 和实际部署另按最终构建哈希核验，不能仅因注册版本同为 0.1.20 就声称远端已包含 #13、#14。
