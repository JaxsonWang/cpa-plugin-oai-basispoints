package basispoints

import (
	"encoding/json"
	"sort"
	"strings"
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
// 填入执行请求；WS 开关读取当前文件，代理同时绑定文件设置与宿主全局策略。
// 原生通道的过载、限流冷却（unavailable）不代表 Basis Points 通道不可用，不据此跳过。
func (s *Service) withHostCredential(request ExecutorRequest) (ExecutorRequest, error) {
	s.mu.RLock()
	proxyURL, ready := s.hostProxyURL, s.hostReady
	s.mu.RUnlock()
	if !ready {
		return request, fail(503, "host_config_unavailable", "CPA has not supplied its transport policy through model.static")
	}
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
	sort.Slice(candidates, func(i, j int) bool {
		left, right := strings.ToLower(candidates[i].Name), strings.ToLower(candidates[j].Name)
		if left == right {
			return candidates[i].Index < candidates[j].Index
		}
		return left < right
	})
	// 只推进成功选中的账号，过期文件不能把额外流量转移给其后的同一账号。
	// 选择过程串行化；网络生成不持有此锁，多个账号仍可并发请求。
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	start := 0
	for i, entry := range candidates {
		if entry.Index == s.lastCredential {
			start = (i + 1) % len(candidates)
			break
		}
	}
	errorsByIndex := make([]error, len(candidates))
	for offset := range candidates {
		index := (start + offset) % len(candidates)
		entry := candidates[index]
		var file hostAuthFile
		if err := s.call("host.auth.get", map[string]any{"auth_index": entry.Index, "host_callback_id": request.HostCallbackID}, &file); err != nil {
			errorsByIndex[index] = fail(503, "auth_unavailable", "CPA credential is unavailable: "+safeError(err))
			continue
		}
		var settings struct {
			Type     string `json:"type"`
			Disabled bool   `json:"disabled"`
			ProxyURL string `json:"proxy_url"`
		}
		if err := json.Unmarshal(file.JSON, &settings); err != nil {
			errorsByIndex[index] = fail(400, "invalid_auth", "OAuth credential is not valid JSON")
			continue
		}
		if settings.Disabled || settings.Type != AuthProviderID {
			errorsByIndex[index] = fail(503, "auth_unavailable", "CPA credential has been disabled or changed provider")
			continue
		}
		c, err := parseCredential(file.JSON)
		if err != nil {
			errorsByIndex[index] = err
			continue
		}
		if !c.ExpiresAt.IsZero() && !time.Now().Before(c.ExpiresAt) {
			errorsByIndex[index] = fail(401, "auth_expired", "ChatGPT OAuth access token has expired")
			continue
		}
		selected := request
		selected.AuthID = entry.Index
		selected.AuthProvider = AuthProviderID
		selected.StorageJSON = append([]byte(nil), file.JSON...)
		effectiveProxy := strings.TrimSpace(settings.ProxyURL)
		if effectiveProxy == "" {
			effectiveProxy = strings.TrimSpace(proxyURL)
		}
		selected.credentialProxy = &effectiveProxy
		selected.AuthMetadata = nil
		selected.AuthAttributes = map[string]string{"basispoints_proxy_url": effectiveProxy}
		s.lastCredential = entry.Index
		return selected, nil
	}
	// 按稳定顺序报告真实失败原因，不让轮询游标改变同一故障的 HTTP 分类。
	return request, errorsByIndex[0]
}
