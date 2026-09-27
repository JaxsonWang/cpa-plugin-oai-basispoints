package basispoints

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const sourceAuthAPI = "/v0/management/oai-basispoints/source-auths"

//go:embed source_auth_page.html
var sourceAuthPage string

type sourceAuthEntry struct {
	Index      string `json:"auth_index"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Path       string `json:"path"`
	Runtime    bool   `json:"runtime_only"`
	Disabled   bool   `json:"disabled"`
	Websockets bool   `json:"websockets"`
}

type sourceAuthJSON struct {
	Name string          `json:"name"`
	Path string          `json:"path"`
	JSON json.RawMessage `json:"json"`
}

type sourceAuthManagementRequest struct {
	Method     string `json:"Method"`
	Path       string `json:"Path"`
	Body       []byte `json:"Body"`
	CallbackID string `json:"host_callback_id"`
}

func (s *Service) registerSourceAuthManagement(raw []byte) (any, error) {
	var request struct{ ResourceBasePath string }
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(request.ResourceBasePath, "/v0/resource/plugins/") {
		return nil, fail(400, "invalid_resource_path", "host resource path is required")
	}
	s.mu.Lock()
	s.authPage = strings.TrimRight(request.ResourceBasePath, "/") + "/source-auths"
	s.mu.Unlock()
	return map[string]any{
		"routes": []map[string]string{
			{"Method": "GET", "Path": sourceAuthAPI},
			{"Method": "PATCH", "Path": sourceAuthAPI},
		},
		"resources": []map[string]string{{"Path": "source-auths", "Menu": "Basis Points 源认证", "Description": "编辑原凭证的 WebSockets 字段；不改变 auto 规则。"}},
	}, nil
}

func sourceAuthResponse(status int, body any) map[string]any {
	return map[string]any{"StatusCode": status, "Headers": http.Header{
		"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"},
		"X-Content-Type-Options": {"nosniff"},
	}, "Body": jsonBytes(body)}
}

func (s *Service) handleSourceAuthManagement(raw []byte) (any, error) {
	var request sourceAuthManagementRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fail(400, "invalid_request", "invalid management request")
	}
	s.mu.RLock()
	page := s.authPage
	s.mu.RUnlock()
	// 公开资源只提供静态界面；账号读取和写入始终经过 CPA 的管理鉴权。
	if request.Method == http.MethodGet && page != "" && request.Path == page {
		_, script, _ := strings.Cut(sourceAuthPage, "<script>")
		script, _, _ = strings.Cut(script, "</script>")
		digest := sha256.Sum256([]byte(script))
		return map[string]any{"StatusCode": 200, "Body": []byte(sourceAuthPage), "Headers": http.Header{
			"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"},
			"X-Content-Type-Options": {"nosniff"}, "Referrer-Policy": {"no-referrer"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"},
		}}, nil
	}
	if request.Path != sourceAuthAPI || (request.Method != http.MethodGet && request.Method != http.MethodPatch) {
		return sourceAuthResponse(404, map[string]string{"error": "not found"}), nil
	}
	var result any
	var err error
	if request.Method == http.MethodGet {
		result, err = s.listSourceAuthSettings(request.CallbackID)
	} else {
		result, err = s.saveSourceAuthSettings(request)
	}
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			return sourceAuthResponse(api.Status, map[string]string{"error": api.Message}), nil
		}
		// 不回显宿主错误正文，避免其中包含凭证 JSON、令牌或服务器路径。
		return sourceAuthResponse(502, map[string]string{"error": "源认证操作失败；请检查 CPA 日志，勿重复提交或分享凭证内容。"}), nil
	}
	return sourceAuthResponse(200, result), nil
}

func (s *Service) sourceAuthEntries(callbackID string) ([]sourceAuthEntry, error) {
	var response struct {
		Files []sourceAuthEntry `json:"files"`
	}
	if err := s.call("host.auth.list", map[string]any{"host_callback_id": callbackID}, &response); err != nil {
		return nil, err
	}
	s.mu.RLock()
	dir := s.authDir
	s.mu.RUnlock()
	if dir == "" {
		return nil, fail(503, "auth_dir_unavailable", "尚未获得源认证目录，请在 CPA 加载 Codex 凭证后刷新。")
	}
	entries := make([]sourceAuthEntry, 0)
	seen := make(map[string]bool)
	for _, entry := range response.Files {
		// 编辑入口只支持宿主认证根目录的原生文件，不接受子记录或任意路径。
		if entry.Provider != AuthProviderID || entry.Runtime || entry.Index == "" ||
			entry.Name == "" || strings.ContainsAny(entry.Name, "/\\") ||
			!strings.HasSuffix(strings.ToLower(entry.Name), ".json") ||
			filepath.Clean(entry.Path) != filepath.Join(dir, entry.Name) || seen[entry.Name] {
			continue
		}
		seen[entry.Name] = true
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (s *Service) readSourceAuth(entry sourceAuthEntry, callbackID string) (map[string]json.RawMessage, error) {
	var source sourceAuthJSON
	if err := s.call("host.auth.get", map[string]any{"auth_index": entry.Index, "host_callback_id": callbackID}, &source); err != nil {
		return nil, err
	}
	if source.Name != entry.Name || filepath.Clean(source.Path) != filepath.Clean(entry.Path) {
		return nil, fail(409, "auth_changed", "源认证位置已变化，请刷新列表后重试。")
	}
	return parseSourceAuthFields(source.JSON)
}

func parseSourceAuthFields(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fail(422, "invalid_source_auth", "源认证不是有效的 JSON 对象。")
	}
	var provider string
	if json.Unmarshal(fields["type"], &provider) != nil || provider != AuthProviderID {
		return nil, fail(409, "auth_changed", "源认证类型已变化，请刷新列表后重试。")
	}
	return fields, nil
}

func (s *Service) listSourceAuthSettings(callbackID string) (any, error) {
	entries, err := s.sourceAuthEntries(callbackID)
	if err != nil {
		return nil, err
	}
	files := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		fields, err := s.readSourceAuth(entry, callbackID)
		if err != nil {
			return nil, err
		}
		// 仅返回界面需要的字段，绝不把完整源 JSON 或令牌传给浏览器。
		files = append(files, map[string]any{
			"auth_index": entry.Index, "name": entry.Name, "disabled": entry.Disabled,
			"websockets":         credentialWebsocketsEnabled(ExecutorRequest{StorageJSON: jsonBytes(fields)}),
			"runtime_websockets": entry.Websockets,
		})
	}
	return map[string]any{"files": files}, nil
}

func (s *Service) saveSourceAuthSettings(request sourceAuthManagementRequest) (any, error) {
	var patch struct {
		Index      string `json:"auth_index"`
		Websockets *bool  `json:"websockets"`
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&patch) != nil || decoder.Decode(new(any)) != io.EOF || strings.TrimSpace(patch.Index) == "" || patch.Websockets == nil {
		return nil, fail(400, "invalid_patch", "必须提供 auth_index 和布尔值 websockets，不接受其他字段。")
	}
	s.authEditMu.Lock()
	defer s.authEditMu.Unlock()
	entries, err := s.sourceAuthEntries(request.CallbackID)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Index != patch.Index {
			continue
		}
		// 保存时重新读取源文件，不使用页面打开时缓存的 OAuth 令牌快照。
		info, err := os.Lstat(entry.Path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, fail(409, "auth_changed", "源认证文件不存在或不是普通文件，请刷新列表。")
		}
		raw, err := os.ReadFile(entry.Path)
		if err != nil {
			return nil, fail(500, "source_read_failed", "无法读取源认证文件；未保存。")
		}
		fields, err := parseSourceAuthFields(raw)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(bytes.TrimSpace(fields["websockets"]), jsonBytes(*patch.Websockets)) {
			return map[string]any{"saved": true, "changed": false, "websockets": *patch.Websockets}, nil
		}
		fields["websockets"] = jsonBytes(*patch.Websockets)
		if err := writeSourceAuthFields(entry.Path, jsonBytes(fields)); err != nil {
			return nil, fail(500, "source_write_failed", "源认证写回失败；原文件未替换，请检查目录权限。")
		}
		return map[string]any{"saved": true, "changed": true, "websockets": *patch.Websockets}, nil
	}
	return nil, fail(404, "auth_not_found", "源认证不存在或已重新加载，请刷新列表。")
}

// 原子替换源文件，由 CPA 原有文件监控重新生成全部派生认证。
// 不调用 host.auth.save，避免其重新构建认证时改写其他元数据。
func writeSourceAuthFields(path string, payload []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".basispoints-ws-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
