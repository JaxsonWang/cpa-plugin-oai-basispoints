# PR #20 host 凭据模式优化验收

日期：2026-09-30（UTC+8）。基于 PR #20 提交 `aba46849e389f90c7842f220c24dd5714d793335`，仅修改插件。原工作区已有其他未提交改动，本次在 `codex/pr20-host-credentials` 隔离工作树完成，未合并、推送或部署。

## 实施范围

- 保留 PR 的原生凭据归属：host 模式不接管 `auth.parse`，不展开虚拟认证，不写 OAuth 文件，也不另行刷新 refresh token。
- 使用现有 `model.static` 接收宿主全局代理。HTTP、SSE、图片上传与 WS 均按凭据代理优先、宿主全局代理其次的规则出站；不因代理失败而直连。
- 未修改的 CPA `host.http.*` JSON ABI 无法给 self 路由重新绑定账号代理，因此 host 模式使用插件内绑定当前凭据的 HTTP 客户端。账号之间隔离连接池，代理变化时替换该账号的连接池；令牌仍逐次从宿主读取。
- 以实际选中的有效账号推进轮询，修复过期文件导致的流量偏斜；全部失败时按稳定文件顺序保留具体错误分类。
- 删除独立设置页面、管理接口及对应前端测试/CI 步骤。使用 CPA 原生凭证设置管理 `websockets`，不修改 CPA 管理前端或主程序。
- `credential_source` 默认仍为 `virtual`；使用本次长期凭据维护路径需明确配置为 `host`。

## 静态与本地 socket 验证

以下命令通过：

```bash
go test -race ./... -count=1 -timeout=120s
go vet ./...
go mod tidy -diff
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
git diff --check
```

另外检查了 gofmt，并重复执行 20 次活动 HTTP 停用测试。新增用例覆盖：

- HTTP / SSE / WS / WS 握手失败后 HTTP 路径的全局代理、凭据代理覆盖及显式 direct。
- 图片上传与生成走同一代理；401/403/407/429、非法或不可达代理不换号、不直连；错误不回显代理密码。
- 收到响应头之前的请求取消、超时、插件停用、响应体上限与不自动重放重定向。
- 同账号连续三个 HTTP 请求只建立一个 TCP 连接。
- 含过期账号时，60 次并发凭据选择对两个有效账号各分配 30 次。
- 已保存的新 token、WS 开关、禁用状态和 provider 变化在读取当前文件时生效。

## 未修改 CPA 的真实 ABI 验证

宿主：干净的 CPA `v7.3.19` 源码，提交 `6dea3dfa`，Go 1.26.2，macOS ARM64；以 `-mod=readonly` 编译到独立临时目录。没有改动宿主源码、测试、配置文件或现有服务。所有认证、宿主运行配置、本地上游与代理均为临时合成夹具。

使用仓库中的 `scripts/verify_host_credentials.py`，传入已构建的 CPA 程序和插件动态库执行。以下 10 项通过：

| 检查 | 结果 |
| --- | --- |
| 只产生一条原生 Codex 认证 | 通过，没有展开为两条 |
| 插件旧源认证管理接口 | HTTP 404，不再注册 |
| 全局代理 HTTP | 通过 |
| 全局代理 SSE | 通过 |
| 凭据代理覆盖全局代理 | 通过 |
| CPA 原生管理接口开启 WS | 下一次请求使用 WS |
| 清空凭据代理后的全局代理 WS | 通过 |
| 宿主文件保存新 access token | 下一次请求使用新值 |
| CPA 重启 | 使用已保存的新 token 和 WS 设置 |
| CPA 原生管理接口关闭 WS | 下一次请求回到 HTTP |

八次生成各执行一次，代理共接收到全局通道 6 次、凭据通道 2 次请求。最终构建及验测证据：

- 插件 SHA256：`98f267e0413c788c0fd18dd367d840c5d1204990668e733166b4dacf18aa8b97`。
- 本机临时证据目录：`/var/folders/h5/wwxhzbts0m36rzvv3f4srn0r0000gn/T/basispoints-host-acceptance-pi63ytu_/`，包含 `summary.json` 与宿主运行日志。

## 验证边界

- 使用合成 OAuth 字段和本地上游；通过 CPA 原生管理接口更新 access token 验证读取与重启，不等同于调用真实 OAuth 刷新端点。PR 作者此前报告的真实刷新验证不作为本轮独立验收结论。
- 没有操作 NAS 线上服务，也未执行 Linux/Windows 现场验收。
- self 路由仍不经过 CPA 原生认证调度器，不能声称继承了原生账号级优先级、冷却、用量归属或 `host.http.*` 请求日志捕获；这些限制没有通过伪造身份、错误类别或成功结果绕过。
