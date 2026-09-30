# 默认 host 凭据模式验收

日期：2026-09-30（UTC+8）。本次将插件默认凭据来源改为 `host`，不修改 CPA 主程序，不发布新版本，也不自动修改现有服务配置或部署。

## 行为与配置边界

- 新服务、未配置 `credential_source`、空值及仅含空白的值均使用 `host`。不再默认接管 Codex 文件解析，凭据刷新和持久化由 CPA 原生负责。
- 显式 `virtual` 仍然有效；已有 YAML 或 `data_dir/settings.json` 中保存的选择不自动迁移。持久化字段继续覆盖 YAML；删除字段后才采用新默认值。
- 旧设置文件没有该字段时采用 `host`；重新配置时同样执行上述规则。能力声明、模型路由与实际凭据模式保持一致。
- 示例配置、注册字段说明和 README 同步更新。协议测试夹具显式声明其直接注入认证所使用的 `virtual`，默认 host 的测试不依赖这些夹具。
- `stream_tool_mode` 继续默认 `incremental`。`buffered` 是可靠性与首字延迟的选择：所有允许调用工具的回合都等待整轮校验，即使最终只返回正文；不增加重生成次数，不修补无效参数，不保证模型永不生成非法 JSON。

## 精确交付源码验证

使用独立 Git 索引准备本次交付源码，未包含工作区中另一项 WS 断流诊断改动、`.gitignore` 修改或已经暂存的 `AGENTS.md` 删除。原索引和其他工作区改动均保留。

| 检查 | 结果 |
| --- | --- |
| `go test -race -count=1 -json ./...` | 932 项测试及子测试通过，0 失败 |
| `go vet ./...`、`go mod tidy -diff` | 通过，无依赖变化 |
| 未修改 CPA v7.3.19 + 默认 host 的原生 ABI 联调 | 10 项通过 |
| 未修改 CPA v7.3.19 + 默认 host 的 #21 原生联调 | 8 项通过 |
| macOS ARM64 原生动态库构建 | 通过 |
| Linux AMD64 CGO 交叉构建及 ELF 检查 | 通过 |

host 联调夹具不再写入 `credential_source: host`，因此直接覆盖默认值。验证一条原生认证、旧插件管理端点未注册、全局/账号代理、HTTP/SSE/WS、CPA 原生凭据 WS 开关、更新后的 token 读取及重启后读取，共 10 项。

#21 联调同样未指定凭据来源，覆盖：增量模式真实流内失败；缓冲正文/摘要后一次重生成恢复；连续失败仍返回 422；上游请求错误保持 400 且不重生成；无工具不缓冲；下游 WS 的恢复及耗尽。全部通过，未用成功终态掩盖错误。

工作区完整快照另有 955 项测试及子测试通过；其中包含未纳入本次交付的诊断用例，不能把该数字当成本次提交的测试数。

## Linux AMD64 构建

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
  CC='/opt/homebrew/bin/zig cc -target x86_64-linux-gnu.2.17' \
  go build -mod=readonly -buildvcs=false -trimpath \
  -ldflags='-s -w -buildid=' -buildmode=c-shared \
  -o oai-basispoints.so ./cmd/basispoints
```

- 最终含中文配置说明候选的 SHA256：`5e4c3d25acb2592460f0b3647e4a3760a910b73c2753e69e2a7a32cf062c3b05`。
- 格式：ELF64、little-endian、AMD64、共享库；导出原生插件入口，无可执行栈、RPATH、RUNPATH 或 TEXTREL。
- 版本号仍为 `0.2.8`，这是本地候选，不是新增 GitHub Release。

证据目录：`build/host-default-20260930-YCX5uD/`。`delivery-source/` 是精确验测快照，`delivery-tests.jsonl`、`delivery-native-acceptance.txt`、`issue21-host-default.jsonl` 分别记录全量测试与两组原生验收。

## 配置说明简体中文补充

随后根据配置面板截图，将超时、响应体上限、认证模式、工具目录版本及接口地址的说明改为简体中文，插件简介一并统一；不改字段名、配置值或执行逻辑。注册元数据共 13 项说明（插件简介及 12 个字段）均含中文，不再保留整句英文解释。

此文案补充以 `localized-source/` 留存，重新执行凭据/配置定向测试及 10 项原版 CPA 原生验收，全部通过，并重新构建 macOS ARM64 与 Linux AMD64 动态库。`localized-native-acceptance.txt` 留存结果。932 项全量回归和 8 项缓冲验收对应补充文案前的同一执行逻辑；中文文案没有在 NAS 管理页面做部署后的视觉验收。

## 既有部署的只读烟测

本次另对用户此前已更新的 NAS 服务执行 4 项串行请求：普通文本、SSE 文本、带中文及 JSON 转义的 function 调用、工具结果回传。全部返回 HTTP 200，终态与用量完整；SSE 没有错误事件，参数精确匹配，工具只返回合成结果而未执行外部动作。未改动服务配置、凭据或客户端配置，未重启服务。

证据为上述目录中的 `live-smoke/summary.json`。此结果仅覆盖现有部署；新构建的默认 host 候选未在本轮部署，不能用这 4 项结果证明新二进制已经上线。

## 验证限制

- 原生运行验证在 macOS ARM64 上完成；Linux AMD64 只完成交叉构建与静态格式检查，没有在 Linux 进程中加载本次新库。
- 本地上游、代理和 OAuth 字段为合成夹具；保存新 token 后读取通过，不等于本轮调用了真实 OAuth 刷新端点。
- 既有线上缓冲验测见 [#21 线上记录](issue-21-live-20260930.md)。本次新默认值不会改变已经加载的旧动态库，也不会覆盖现有显式配置；不能把旧候选的线上结果称为新 host 默认值的部署验收。
- 原生认证调度、冷却、账号级用量归属及宿主 HTTP 日志路径的限制仍见 README；没有修改宿主或通过伪造认证、错误和成功结果绕过。
