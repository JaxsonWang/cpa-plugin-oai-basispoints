# 按 AuthID 强制 HTTP 的本地验测

日期：2026-09-27（UTC+8）。基于主分支 `f351191`，保留 PR #13、PR #14 及既有 WS 安全边界。

## 改动范围

- 新增可选 `http_only_auth_ids`，精确匹配 CPA 传给执行器的 `AuthID`，仅改变 Basis Points 插件的上游传输。
- 不改 CPA 主程序、源凭据、原生 Codex WS 开关、会话亲和性或账号选择。
- 空列表保持原行为；当前凭据 `websockets` 关闭、缺失或无效时，仍禁止 WS。列表不能强制开启 WS。
- 维持 `settings.json` 对 YAML 的既有覆盖优先级，不引入自动冷却、能力缓存、换号、重放或额外探测。
- 使用方法与持久化注意事项见 `../configuration/http-only-auth.md`。

## 先复现再实现

新增测试通过公开 YAML 配置入口传入名单，并使用真实本地 WebSocket 服务器计数。旧实现中，匹配名单和去空白名单的流式/非流式四个子用例均失败：预期零次 WS，实际仍发生一次握手。实现后通过，不是仅以“字段不存在导致编译失败”代替行为复现。

## 验证结果

| 验证项 | 结果 |
| --- | --- |
| 干净源码全量回归 | `go test -race -count=1 -json ./...`，**667 项测试/子测试通过** |
| 新功能随机顺序 | `-race -shuffle=on -count=3` 通过 |
| 静态检查 | vet、gofmt、`go mod tidy -diff`、actionlint、差异检查通过 |
| 匹配与权限门槛 | 流式/非流式、精确 ID、非前缀/非通配符、大小写、非 account_id/会话 ID、空名单、全局 HTTP、关闭/缺失/无效凭据开关通过 |
| 配置一致性 | 去两端空白、去重、无效配置拒绝且不覆盖当前配置、状态快照深拷贝、持久化恢复和优先级通过 |
| 隔离与可撤销 | 同一实例中两份 AuthID 并发路由互不污染；删除覆盖后，同一 AuthID 恢复 WS |
| 原生认证保持 | 源凭据字节及原生/虚拟记录的原 WS 开关保持不变 |
| 原版 CPA 新门禁 | CPA v7.3.17 实际加载新动态库，五种配置 × 流式/非流式，共 **10 项通过** |
| 原版 CPA 旧回归 | WS **107 项**、HTTP **96 项**通过；连同新门禁共 **213 项** |
| 原生客户端 | Codex 0.158.0-alpha.2.1 补丁增删改、1 个 fileChange、3 个差异事件，以及时钟/休眠/异步答案往返通过 |

实际宿主的新门禁结果：

- `http_only_auth_ids: [bp-fixture]` 命中当前虚拟认证：每个请求 **WS 0 次、HTTP 1 次**。
- 名单只有其他 ID、当前凭据开启：每个请求 **WS 1 次、HTTP 0 次**。
- 当前凭据关闭、缺失开关，或者全局模式为 HTTP：每个请求 **WS 0 次、HTTP 1 次**。
- 各实例的源凭据文件哈希不变。

宿主、插件动态库与客户端真实运行，但推理上游使用本地合成夹具。本轮没有使用真实 OAuth 凭据进行生成，也没有将本地回归写成 NAS 实机验收。

## 本地包

- 注册版本保持 **0.1.20**，本轮不创建新标签或 Release；用哈希区分本候选与此前同版本包。
- 源码清单：**49 个 Go/模块文件**，与验测及编译源码逐字一致。
- Linux amd64 `c-shared`，7,922,024 字节；ELF64 / EM_X86_64 / ET_DYN。
- ABI 导出 `cliproxy_plugin_init`、`cliproxyPluginCall` 存在；GLIBC 版本依赖仅 2.2.5、2.3.2。
- SO SHA-256：`c6b6caa9656981b174e46de8a6d617e9ee95ea41d57c746be02268db9a9782ba`。
- ZIP 仅包含 SO，解包后逐字一致。

构建和完整证据保存在忽略目录 `build/http-only-auth-20260927-58_wn622/`：`before-regression.log`、`full-race.log`、`test-summary.json`、`host-checks.json`、各宿主结果、`artifact-verification.json`、`source-manifest.json`、`SHA256SUMS`。

## 交付边界

只交付源码和本地候选，不自动给生产凭据 B 添加覆盖、不替换 NAS 文件、不重启容器，也不修改源凭据开关。默认名单为空，因此仅安装新包不会自动改变任何凭据的策略。部署时需选择实际 AuthID 并核对最终生效配置，尤其注意已有持久化文件的优先级。

用户原有 README 未提交改动保留，不纳入本次提交；CPA 工作区状态与改动前一致。
