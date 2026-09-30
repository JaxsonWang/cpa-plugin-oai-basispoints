package basispoints

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

type Service struct {
	attachments       attachmentCache
	mu                sync.RWMutex
	cfg               Config
	host              HostCall
	stopped           bool
	streams           map[*runningStream]struct{}
	streamWG          sync.WaitGroup
	requests          map[string]*requestScope
	hostProxyURL      string
	hostReady         bool
	credentialMu      sync.Mutex
	lastCredential    string
	credentialClients map[string]credentialHTTPClient
}

func NewService() *Service {
	cfg := defaultConfig()
	return &Service{cfg: cfg}
}

func (s *Service) SetHost(host HostCall) {
	s.mu.Lock()
	s.host = host
	s.mu.Unlock()
}

func (s *Service) call(method string, payload any, out any) error {
	s.mu.RLock()
	host := s.host
	s.mu.RUnlock()
	if host == nil {
		return errors.New("host callback is not initialized")
	}
	return host(method, payload, out)
}

func (s *Service) configure(raw json.RawMessage) error {
	cfg := defaultConfig()
	var request struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return fail(400, "invalid_config", "plugin configuration request is invalid")
		}
	}
	if len(request.ConfigYAML) > 0 {
		if err := unmarshalYAML(request.ConfigYAML, &cfg); err != nil {
			return fail(400, "invalid_config", "plugin configuration is invalid: "+err.Error())
		}
	}
	// Persist only non-secret settings. Token material always remains in CPA's
	// auth store and is supplied in ExecutorRequest.StorageJSON.
	if cfg.DataDir != "" {
		if data, err := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json")); err == nil {
			_ = json.Unmarshal(data, &cfg)
		}
	}
	if err := cfg.normalize(); err != nil {
		return err
	}
	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
			return fail(500, "config_storage", "cannot create plugin data directory")
		}
		data, _ := json.MarshalIndent(cfg, "", "  ")
		_ = os.WriteFile(filepath.Join(cfg.DataDir, "settings.json"), data, 0600)
	}
	s.mu.Lock()
	s.cfg = cfg
	s.stopped = false
	s.mu.Unlock()
	return nil
}

func (s *Service) Handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := s.configure(raw); err != nil {
			return nil, err
		}
		return registration(s.config()), nil
	case "plugin.quiesce":
		s.stopStreams()
		return map[string]any{}, nil
	case "auth.identifier":
		// CPA 按这个标识把文件认证交给插件解析；Basis Points 使用
		// Codex OAuth 文件中的访问令牌。
		return map[string]any{"identifier": AuthProviderID}, nil
	case "executor.identifier":
		return map[string]any{"identifier": Provider}, nil
	case "auth.parse":
		var request authParseRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		if s.hostCredentialMode() {
			return s.authParseHostMode(), nil
		}
		return parseAuthRequest(request)
	case "model.route":
		return s.routeModel(raw)
	case "request.intercept_before":
		return map[string]any{}, nil
	case "request.intercept_after":
		return s.interceptUpstreamRequest(raw)
	case "request.complete":
		return s.completeUpstreamRequest(raw)
	case "auth.login.start":
		return nil, fail(400, "login_unavailable", "Import an existing CPA codex OAuth credential; interactive login is not used")
	case "auth.login.poll":
		return map[string]any{"Status": "error", "Message": "Import an existing CPA codex OAuth credential"}, nil
	case "auth.refresh":
		return authRefresh(raw)
	case "model.static":
		var request struct {
			Host *struct{ ProxyURL string }
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, fail(400, "invalid_request", "invalid static model request")
		}
		if request.Host != nil {
			s.mu.Lock()
			s.hostProxyURL = request.Host.ProxyURL
			s.hostReady = true
			s.mu.Unlock()
		}
		return modelRegistration(s.config()), nil
	case "model.register", "model.for_auth":
		return modelRegistration(s.config()), nil
	case "response.intercept_after":
		return s.interceptModelCatalog(raw)
	case "executor.execute":
		return s.execute(raw, false)
	case "executor.execute_stream":
		return s.execute(raw, true)
	case "executor.count_tokens":
		return nil, fail(400, "unsupported_token_count", "oai-basispoints does not provide an accurate standalone token count")
	case "executor.http_request":
		return nil, fail(400, "unsupported_method", "use the Basis Points model executor")
	case "plugin.shutdown":
		s.stopStreams()
		return map[string]any{}, nil
	default:
		return nil, fail(400, "unsupported_method", "unsupported plugin method: "+method)
	}
}

func (s *Service) execute(raw json.RawMessage, stream bool) (any, error) {
	s.mu.RLock()
	stopped := s.stopped
	s.mu.RUnlock()
	if stopped {
		return nil, fail(503, "plugin_stopped", "oai-basispoints is shut down")
	}
	var request ExecutorRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fail(400, "invalid_request", "executor request is invalid")
	}
	if s.hostCredentialMode() {
		var err error
		if request, err = s.withHostCredential(request); err != nil {
			return nil, err
		}
	}
	body, credential, err := s.prepareRequest(request)
	if err != nil {
		return nil, err
	}
	if stream {
		return s.executeStream(request, body, credential)
	}
	payload, response, headers, err := s.executeResponse(request, body, credential)
	if err != nil {
		return nil, err
	}
	if request.Format == "codex" {
		payload = codexTerminalResponse(response)
	}
	return map[string]any{"Payload": payload, "Headers": headers}, nil
}

// 在交付任何客户端数据前完成全量校验，畸形调用只允许重生成一次。
func (s *Service) executeResponse(request ExecutorRequest, body map[string]any, credential credential) ([]byte, map[string]any, http.Header, error) {
	run, err := s.startRun(request)
	if err != nil {
		return nil, nil, nil, err
	}
	defer run.finish()
	request.run = run
	source, err := executorSource(request)
	if err != nil {
		return nil, nil, nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		response, selected, wsErr := s.tryWebSocket(request, body, credential, run, nil)
		if wsErr != nil {
			return nil, nil, nil, wsErr
		}
		var headers http.Header
		if !selected {
			if err := run.contextError(); err != nil {
				return nil, nil, nil, err
			}
			upstream, openErr := s.upstreamRequest(request, body, credential, false)
			if openErr != nil {
				return nil, nil, nil, openErr
			}
			headers = upstream.Headers
			if len(upstream.Body) > s.config().MaxResponseBytes {
				return nil, nil, nil, fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
			}
			var parseErr error
			response, parseErr = parseResponse(upstream.Body, headers)
			if parseErr != nil {
				return nil, nil, nil, parseErr
			}
		}
		payload, transformed, _, transformErr := transformResponseBody(jsonBytes(response), source)
		if transformErr == nil {
			// 结果已重新编码，不能继续使用上游 SSE/压缩/长度等实体头。
			resultHeaders := headers.Clone()
			if resultHeaders == nil {
				resultHeaders = make(http.Header)
			}
			resultHeaders.Set("Content-Type", "application/json")
			for _, name := range []string{"Content-Length", "Content-Encoding", "Transfer-Encoding", "ETag"} {
				resultHeaders.Del(name)
			}
			return payload, transformed, resultHeaders, nil
		}
		var apiError *APIError
		if !errors.As(transformErr, &apiError) || apiError.Kind != "invalid_tool_call" || attempt != 0 || response["status"] == "incomplete" {
			return nil, nil, nil, transformErr
		}
		retry := cloneObject(body)
		items, _ := body["input"].([]any)
		retry["input"] = appendBeforeCompaction(append([]any{}, items...), []any{messageItem("developer", transportRetryHint+" Diagnostic: "+apiError.Message)})
		body = retry
	}
	return nil, nil, nil, relayError("retry_exhausted")
}

func (s *Service) status() map[string]any {
	cfg := s.config()
	s.mu.RLock()
	stopped := s.stopped
	s.mu.RUnlock()
	return map[string]any{
		"provider":                     Provider,
		"version":                      Version,
		"responses_url":                cfg.ResponsesURL,
		"upstream_model":               cfg.UpstreamModel,
		"models":                       cfg.Models,
		"model_mappings":               cfg.ModelMappings,
		"upstream_transport":           cfg.UpstreamTransport,
		"stream_tool_mode":             cfg.StreamToolMode,
		"ws_handshake_timeout_seconds": cfg.WSHandshakeTimeoutSeconds,
		"stopped":                      stopped,
		"reasoning_efforts":            []string{"low", "medium", "high", "xhigh", "ultra"},
	}
}

func registration(cfg Config) map[string]any {
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{
			"Name":             "OpenAI Basis Points",
			"Version":          Version,
			"Author":           "jaxson-wang",
			"GitHubRepository": "https://github.com/JaxsonWang/cpa-plugin-oai-basispoints",
			"Description":      "通过 CPA 的 Codex OAuth 凭据接入 Basis Points，安全转发客户端工具调用。",
			"ConfigFields": []map[string]any{
				{"Name": "upstream_transport", "Type": "string", "Description": "auto：仅凭据 websockets 已开启时优先 WS，握手失败可回退 HTTP/SSE；http：仅使用 HTTP/SSE。"},
				{"Name": "stream_tool_mode", "Type": "string", "Description": "incremental（默认）：实时交付正文和摘要；buffered：本轮有可调用工具时等待整轮校验，可在交付前重生成一次，但首字延迟增大。"},
				{"Name": "ws_handshake_timeout_seconds", "Type": "integer", "Description": "WS 单次握手上限，默认 5 秒；每轮生成只尝试一次。"},
				{"Name": "responses_url", "Type": "string", "Description": "Basis Points 上游 Responses 接口地址，通常无需修改。"},
				{"Name": "upstream_model", "Type": "string", "Description": "未单独配置 model_mappings 的别名使用的上游模型。"},
				{"Name": "models", "Type": "array", "Description": "启用的客户端模型别名列表，数量不限。"},
				{"Name": "model_mappings", "Type": "object", "Description": "客户端别名到实际上游模型的映射；键必须已列入 models。"},
				{"Name": "timeout_seconds", "Type": "integer", "Description": "上游请求超时时间，单位为秒；默认 300，允许范围为 10～1800。"},
				{"Name": "max_response_bytes", "Type": "integer", "Description": "上游响应体大小上限，单位为字节；默认 67108864（64 MiB）。"},
				{"Name": "auth_mode", "Type": "string", "Description": "Basis Points 认证模式，通常保持 chatgpt。"},
				{"Name": "tools_version_id", "Type": "string", "Description": "可选的 Basis Points 工具目录版本 ID，通常留空。"},
				{"Name": "credential_source", "Type": "string", "Description": "host（默认）：Codex 文件交由 CPA 原生刷新和持久化，执行时读取宿主最新凭据；virtual：显式启用插件虚拟认证。"},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":            cfg.CredentialSource != CredentialSourceHost,
			"model_router":             cfg.CredentialSource == CredentialSourceHost,
			"model_provider":           true,
			"executor":                 true,
			"executor_model_scope":     "both",
			"executor_input_formats":   []string{"openai-response", "codex"},
			"executor_output_formats":  []string{"openai-response", "codex"},
			"response_interceptor":     true,
			"request_interceptor":      true,
			"request_lifecycle_plugin": true,
		},
		"config": cfg,
	}
}

func modelRegistration(cfg Config) map[string]any {
	models := make([]map[string]any, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		upstream, _ := cfg.upstreamModelForAlias(model)
		models = append(models, map[string]any{
			"ID":                         model,
			"Object":                     "model",
			"Name":                       upstream,
			"OwnedBy":                    Provider,
			"DisplayName":                model,
			"SupportedGenerationMethods": []string{"responses"},
			"SupportedInputModalities":   []string{"text", "image"},
			"SupportedOutputModalities":  []string{"text"},
			"Thinking":                   map[string]any{"Levels": []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
			"UserDefined":                true,
		})
	}
	return map[string]any{"Provider": Provider, "Models": models}
}

func unmarshalYAML(raw []byte, value any) error {
	// Kept in one function so config parsing is easy to test and the service
	// package does not expose YAML details to the ABI layer.
	return yaml.Unmarshal(raw, value)
}
