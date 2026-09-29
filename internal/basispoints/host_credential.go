package basispoints

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"
)

// credential_source: host 模式下，Codex OAuth 文件完全交还 CPA 原生解析、刷新与持久化。
// 插件不再展开 plugin_virtual 认证，因此宿主会把轮换后的 refresh_token 写回源文件；
// 执行时通过 host.auth.list / host.auth.get 读取宿主当前维护的凭据，而不是解析时的快照。
//
// 背景：未修改的 CPA 对多条展开认证统一标记 plugin_virtual，并在 persist 中跳过这类记录。
// virtual 模式下原生记录刷新后的 refresh_token 只留在内存，CPA 重启会重新加载已被轮换的
// 旧 refresh_token；Basis Points 记录也不会跟随原生刷新，access_token 到期后请求失败。

type hostAuthEntry struct {
	Index    string `json:"auth_index"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Path     string `json:"path"`
	Runtime  bool   `json:"runtime_only"`
	Disabled bool   `json:"disabled"`
}

type hostAuthFile struct {
	JSON json.RawMessage `json:"json"`
}

var hostCredentialCursor atomic.Uint64

func (s *Service) hostCredentialMode() bool {
	return s.config().CredentialSource == CredentialSourceHost
}

// host 模式不接管文件解析，宿主按原生 codex 类型加载、刷新并持久化。
func (s *Service) authParseHostMode() map[string]any {
	return map[string]any{"Handled": false}
}

// host 模式的静态模型没有插件认证可供调度，由路由器把别名请求直接交给本执行器。
func (s *Service) routeModel(raw json.RawMessage) (any, error) {
	if !s.hostCredentialMode() {
		return map[string]any{"Handled": false}, nil
	}
	var request struct {
		RequestedModel string `json:"RequestedModel"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fail(400, "invalid_request", "invalid model route request")
	}
	if _, ok := s.config().upstreamModelForAlias(strings.TrimSpace(request.RequestedModel)); !ok {
		return map[string]any{"Handled": false}, nil
	}
	return map[string]any{"Handled": true, "TargetKind": "self", "Reason": "basispoints_alias"}, nil
}

// withHostCredential 按稳定顺序轮询宿主中未禁用的原生 Codex 认证，把其最新文件内容
// 填入执行请求；后续凭据解析、WS 开关与代理继承沿用 StorageJSON 的既有逻辑。
// 原生通道的过载、限流冷却（unavailable）不代表 Basis Points 通道不可用，不据此跳过。
func (s *Service) withHostCredential(request ExecutorRequest) (ExecutorRequest, error) {
	var listed struct {
		Files []hostAuthEntry `json:"files"`
	}
	if err := s.call("host.auth.list", map[string]any{"host_callback_id": request.HostCallbackID}, &listed); err != nil {
		return request, fail(503, "auth_unavailable", "CPA credential list is unavailable: "+safeError(err))
	}
	candidates := make([]hostAuthEntry, 0, len(listed.Files))
	for _, entry := range listed.Files {
		if entry.Provider == AuthProviderID && !entry.Runtime && !entry.Disabled &&
			entry.Index != "" && entry.Path != "" {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) == 0 {
		return request, fail(503, "auth_unavailable", "no available Codex OAuth credential in CPA")
	}
	start := int(hostCredentialCursor.Add(1)-1) % len(candidates)
	var lastErr error
	for offset := range candidates {
		entry := candidates[(start+offset)%len(candidates)]
		var file hostAuthFile
		if err := s.call("host.auth.get", map[string]any{"auth_index": entry.Index, "host_callback_id": request.HostCallbackID}, &file); err != nil {
			lastErr = fail(503, "auth_unavailable", "CPA credential is unavailable: "+safeError(err))
			continue
		}
		c, err := parseCredential(file.JSON)
		if err != nil {
			lastErr = err
			continue
		}
		if !c.ExpiresAt.IsZero() && !time.Now().Before(c.ExpiresAt) {
			lastErr = fail(401, "auth_expired", "ChatGPT OAuth access token has expired")
			continue
		}
		selected := request
		selected.AuthID = entry.Index
		selected.StorageJSON = append([]byte(nil), file.JSON...)
		return selected, nil
	}
	return request, lastErr
}
