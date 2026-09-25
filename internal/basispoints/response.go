package basispoints

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

// parseResponse 保留完整终态对象，不把失败或不完整响应伪装成 completed。
func parseResponse(raw []byte, headers http.Header) (map[string]any, error) {
	response, err := parseFinalStreamResponse(raw)
	if err == nil {
		return response, nil
	}
	kind := "other"
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(headers.Get("Content-Type"), ";")[0]))
	switch contentType {
	case "application/json", "text/event-stream", "text/html":
		kind = contentType
	case "":
		kind = "missing"
	}
	return nil, fail(502, "invalid_upstream_response", fmt.Sprintf("%s (content_type=%s; bytes=%d)", err.Error(), kind, len(raw)))
}

func terminalResponse(response map[string]any) (map[string]any, error) {
	if response == nil {
		return nil, fail(502, "invalid_upstream_response", "Basis Points returned no response object")
	}
	switch stringValue(response["status"]) {
	case "failed", "cancelled":
		return nil, fail(502, "upstream_response_failed", "Basis Points response failed or was cancelled")
	case "completed", "incomplete":
	default:
		return nil, fail(502, "invalid_upstream_response", "Basis Points response has no valid terminal status")
	}
	if _, ok := response["output"].([]any); !ok {
		return nil, fail(502, "invalid_upstream_response", "Basis Points returned no output array")
	}
	return response, nil
}

// JSON 与 SSE 都按实际响应解码；不重发请求来猜测上游协议。
func parseFinalStreamResponse(raw []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fail(502, "invalid_upstream_response", "Basis Points returned an empty response")
	}
	if trimmed[0] == '{' {
		object, reason := parseRelayObject(string(trimmed))
		if reason != "" {
			return nil, fail(502, "invalid_upstream_response", "Basis Points returned invalid JSON")
		}
		return terminalResponse(object)
	}
	decoder := newSSEDecoder()
	var terminal map[string]any
	emit := func(event, data string) error {
		if strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		object, reason := parseRelayObject(data)
		if reason != "" {
			return fail(502, "invalid_upstream_response", "Basis Points returned invalid SSE JSON")
		}
		kind := stringValue(object["type"])
		if kind == "" {
			kind = event
		}
		switch kind {
		case "error", "response.failed", "response.cancelled":
			return fail(502, "upstream_response_failed", "Basis Points stream reported a failure")
		case "response.completed", "response.incomplete":
			response, err := terminalResponse(objectValue(object["response"]))
			if err != nil {
				return err
			}
			if status := stringValue(response["status"]); status != strings.TrimPrefix(kind, "response.") {
				return fail(502, "invalid_upstream_response", "Basis Points stream terminal status mismatch")
			}
			if terminal != nil {
				return fail(502, "invalid_upstream_response", "Basis Points returned multiple terminal responses")
			}
			terminal = response
		}
		return nil
	}
	if err := decoder.feed(raw, emit); err != nil {
		return nil, err
	}
	// EOF 时处理最后一条没有空行终止的事件，但仍严格校验其中 JSON。
	if err := decoder.feed([]byte{10, 10}, emit); err != nil {
		return nil, err
	}
	if terminal == nil {
		return nil, fail(502, "invalid_upstream_response", "Basis Points stream ended without a terminal response")
	}
	return terminal, nil
}
