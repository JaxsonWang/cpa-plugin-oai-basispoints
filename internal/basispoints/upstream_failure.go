package basispoints

import (
	"encoding/json"
	"fmt"
)

// 只保留协议已知的分类标识；上游自由文本可能包含请求正文或凭据，不能直接转发。
func upstreamFailure(event string, value map[string]any) error {
	response := objectValue(value["response"])
	detail := objectValue(value["error"])
	if response != nil {
		detail = objectValue(response["error"])
	}
	if detail == nil && event == "error" {
		detail = value
	}
	code := stringValue(detail["code"])
	status := upstreamErrorIdentifierStatus(code)
	if status == 0 {
		code = "upstream_response_failed"
	}
	errorType := stringValue(detail["type"])
	switch errorType {
	case "invalid_request", "invalid_request_error", "bad_request_error", "authentication_error", "permission_error", "rate_limit_error", "server_error", "api_error", "overloaded_error":
		if status == 0 {
			status = upstreamErrorIdentifierStatus(errorType)
		}
	default:
		errorType = ""
	}
	// 显式状态优先，尤其不能把携带 invalid_request_error 的 429 降成参数错误。
	if number, ok := value["status"].(json.Number); ok {
		if explicit, err := number.Int64(); err == nil && explicit >= 400 && explicit <= 599 {
			status = int(explicit)
		}
	}
	if status == 0 {
		status = 502
	}
	if errorType == "" {
		errorType = upstreamErrorType(status)
	}
	return &APIError{
		Status: status, Kind: code, Type: errorType,
		Message: fmt.Sprintf("Basis Points upstream failure (event=%s; status=%d; type=%s; code=%s)", event, status, errorType, code),
	}
}

func upstreamErrorIdentifierStatus(identifier string) int {
	switch identifier {
	case "invalid_request", "invalid_request_error", "bad_request_error", "invalid_argument", "invalid_value", "unsupported_value", "invalid_prompt", "context_length_exceeded", "string_above_max_length", "previous_response_not_found", "thinking_signature_invalid", "invalid_encrypted_content", "cyber_policy", "content_policy_violation":
		return 400
	case "message_too_big":
		return 413
	case "authentication_error", "invalid_api_key", "token_expired":
		return 401
	case "permission_error", "permission_denied":
		return 403
	case "model_not_found", "model_not_found_error":
		return 404
	case "rate_limit_error", "rate_limit_exceeded", "insufficient_quota", "usage_limit_reached":
		return 429
	case "server_error", "internal_server_error":
		return 500
	case "overloaded_error", "service_unavailable":
		return 503
	case "api_error":
		return 502
	default:
		return 0
	}
}

func upstreamErrorType(status int) string {
	switch status {
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 429:
		return "rate_limit_error"
	default:
		if status >= 400 && status < 500 {
			return "invalid_request_error"
		}
		return "server_error"
	}
}
