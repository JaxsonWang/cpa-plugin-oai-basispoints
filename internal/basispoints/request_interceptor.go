package basispoints

import (
	"encoding/json"
	"strings"
)

// 使用 CPA 的认证前拦截契约，不通过扩大格式声明来伪装协议转换能力。
type requestInterceptRequest struct {
	SourceFormat   string
	Model          string
	RequestedModel string
}

func (s *Service) interceptUnsupportedProtocol(raw json.RawMessage) (any, error) {
	var request requestInterceptRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fail(400, "invalid_request", "request interceptor payload is invalid")
	}
	if request.SourceFormat != "claude" {
		return map[string]any{}, nil
	}
	cfg := s.config()
	owned := false
	for _, model := range []string{request.Model, request.RequestedModel} {
		// CPA 允许在别名后附加思考等级，且可为凭据加前缀。
		if strings.HasSuffix(model, ")") {
			model, _, _ = strings.Cut(model, "(")
		}
		if _, ok := catalogCanonicalSlug(model, cfg); ok {
			owned = true
			break
		}
	}
	if !owned {
		return map[string]any{}, nil
	}
	return map[string]any{
		"Terminate": true, "StatusCode": 400,
		"ResponseHeaders": map[string][]string{"Content-Type": {"application/json"}},
		"ResponseBody": jsonBytes(map[string]any{"type": "error", "error": map[string]any{
			"type": "invalid_request_error", "code": "unsupported_input_format",
			"message": "oai-basispoints does not support the Claude/Anthropic Messages protocol; use /v1/responses with complete input history, or select a provider that supports /v1/messages",
		}}),
	}, nil
}
