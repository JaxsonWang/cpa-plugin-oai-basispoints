# 仅在 Basis Points 中为指定凭据使用 HTTP/SSE

## 适用情况

CPA 会话已经正常绑定某份凭据，但 Basis Points 对该凭据的 WS 握手返回未启用，而同一凭据的 HTTP 推理仍可用。此时无需改会话 ID、修改 CPA 主程序或切换账号。

新增 `http_only_auth_ids` 只约束本插件的上游传输，不改源凭据的 `websockets`。原生 Codex 同样会读取源凭据开关，因此不应为了仅停用 Basis Points 的 WS 而顺带关闭原生 Codex 的 WS。

## 配置

合并到现有 CPA 插件配置中；以下 `bp-codex-example` 只是占位示例，使用前必须换成当前被选中的真实虚拟认证 AuthID。

```yaml
plugins:
  configs:
    oai-basispoints:
      upstream_transport: auto
      http_only_auth_ids:
        - bp-codex-example
      # 只从 YAML 管理配置时保持为空；详见下方持久化约定。
      data_dir: ""
```

CPA 管理页面的本插件「编辑配置」也会显示 `http_only_auth_ids` 数组字段。它应填写完整的 CPA `AuthID`，可从该模型的认证调度日志 `auth=...` 或虚拟认证记录核对；不是会话 ID、原生 Codex 文件记录 ID、显示名称或 OAuth `account_id`。

- 按完整 ID、区分大小写精确匹配，不支持前缀、通配符或正则表达式。
- 配置项两端空白会去除，重复 ID 会合并；空白 ID 或非列表配置会被拒绝。
- 默认 `[]`，不额外限制任何凭据。删除某个 ID 后，该凭据后续请求恢复既有传输策略，不留失败缓存。
- 缺失或不匹配的请求 AuthID 不会猜测成其他账号；仍按全局传输设置和当前凭据 WS 开关执行。

## 生效规则

| 全局 `upstream_transport` | 当前凭据 `websockets` | 当前 AuthID 在列表中 | 上游传输 |
| --- | --- | --- | --- |
| `http` | 任意 | 任意 | HTTP/SSE |
| `auto` | 关闭、缺失或无效 | 任意 | HTTP/SSE |
| `auto` | 开启 | 是 | HTTP/SSE，零次 WS 握手 |
| `auto` | 开启 | 否 | 一次 WS 握手；之后沿用原有安全回退规则 |

这里 HTTP/SSE 指沿用原通道：流式请求使用 SSE，非流式继续支持既有 HTTP JSON/SSE 响应语义，不是把所有响应强制改成流式。

指定 HTTP 只改变传输，不更换 CPA 选中的凭据、不重写令牌或账号头、不修改会话亲和性。其他凭据仍保持原策略。401/403/407/429 等既有错误处理不变；已建立有效 WS 连接的请求仍不因断流而重放。

## 持久化与调整

保留本插件已有规则：先读 CPA 传入的 YAML，再用 `data_dir/settings.json` 中存在的字段覆盖。若已启用持久化且该 JSON 已有 `http_only_auth_ids`，只修改 YAML 或管理页面字段可能被已有值覆盖。

- 使用 YAML 统一管理时，设置 `data_dir: ""`；这不会删除已有文件，也不影响 OAuth 读取。
- 保留持久化时，应维护有效的 `settings.json` 字段并重新加载插件；不要误把旧持久化值造成的不生效归因于会话绑定。
- 在持久化配置中使用空数组可取消该限制；不要填空字符串作为数组成员。

新字段没有覆盖其他设置的优先级，也没有自动关闭源凭据开关。实际部署前应备份配置，在最终生效配置上验证目标凭据零握手、其他开启凭据仍可握手及源凭据不变。本功能不能让上游未开放的 WS 强制返回 101。
