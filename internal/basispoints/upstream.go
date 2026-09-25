package basispoints

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Service) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.clone()
}

func (s *Service) prepareRequest(request ExecutorRequest) (map[string]any, credential, error) {
	if request.Alt == "responses/compact" {
		return nil, credential{}, fail(400, "unsupported_compaction", "oai-basispoints does not support /responses/compact; send full input history to /responses")
	}
	c, err := credentialFromExecutor(request)
	if err != nil {
		return nil, credential{}, err
	}
	if !c.ExpiresAt.IsZero() && !time.Now().Before(c.ExpiresAt) {
		return nil, credential{}, fail(401, "auth_expired", "ChatGPT OAuth access token has expired")
	}
	source, err := executorSource(request)
	if err != nil {
		return nil, credential{}, err
	}
	cfg := s.config()
	model := stringValue(source["model"])
	if model == "" {
		model = strings.TrimSpace(request.Model)
	}
	source["model"] = model
	source["stream"] = request.Stream
	prepared, err := prepareResponsesBody(source, cfg)
	if err != nil {
		return nil, credential{}, err
	}
	// 先按原始图片计算会话标识，再替换附件引用，避免上传 ID 改变 task/turn。
	if err := s.uploadInputImages(request, prepared, c, cfg); err != nil {
		return nil, credential{}, err
	}
	return prepared, c, nil
}

func authHeaders(c credential, stream bool) http.Header {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	// These headers match the Excel/Basis Points client profile. The access
	// token itself is never logged by this plugin.
	return http.Header{
		"Authorization":           []string{"Bearer " + c.AccessToken},
		"ChatGPT-Account-ID":      []string{c.AccountID},
		"X-OpenAI-Account-ID":     []string{c.AccountID},
		"X-Basispoints-Auth-Mode": []string{c.AuthMode},
		"Content-Type":            []string{"application/json"},
		"Accept":                  []string{accept},
		"Accept-Encoding":         []string{"identity"},
		"Origin":                  []string{"https://bps.openai.com"},
		"X-OpenAI-Internal-Basispoints-Client-Agent-Profile":  []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Editor":         []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Host":           []string{"office"},
		"X-OpenAI-Internal-Basispoints-Client-Platform":       []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Platform-Class": []string{"PC"},
		"X-OpenAI-Internal-Basispoints-Client-Product":        []string{"basispoints-excel-plugin"},
		"X-OpenAI-Internal-Basispoints-Client-Runtime":        []string{"desktop"},
		"X-OpenAI-Internal-Basispoints-Office-Host":           []string{"Excel"},
		"X-OpenAI-Internal-Basispoints-Office-Platform":       []string{"PC"},
		"X-Stainless-Arch":            []string{"unknown"},
		"X-Stainless-Lang":            []string{"js"},
		"X-Stainless-OS":              []string{"Unknown"},
		"X-Stainless-Package-Version": []string{"6.31.0"},
		"X-Stainless-Retry-Count":     []string{"0"},
		"X-Stainless-Runtime":         []string{"browser:chrome"},
		"User-Agent":                  []string{"oai-basispoints/" + Version},
	}
}

func (s *Service) upstreamRequest(request ExecutorRequest, body map[string]any, c credential, stream bool) (upstreamResponse, error) {
	cfg := s.config()
	if cfg.ResponsesURL == "" {
		return upstreamResponse{}, fail(500, "invalid_config", "responses_url is empty")
	}
	payload := map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost,
		"url":              cfg.ResponsesURL,
		"headers":          authHeaders(c, stream),
		"body":             jsonBytes(body),
	}
	var response upstreamResponse
	if err := s.call("host.http.do", payload, &response); err != nil {
		return upstreamResponse{}, fail(502, "upstream_transport", "Basis Points transport failed: "+safeError(err))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, upstreamRequestError(response.StatusCode, response.Body, body, c)
	}
	return response, nil
}

func (s *Service) upstreamStream(request ExecutorRequest, body map[string]any, c credential) (upstreamStream, error) {
	cfg := s.config()
	payload := map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost,
		"url":              cfg.ResponsesURL,
		"headers":          authHeaders(c, true),
		"body":             jsonBytes(body),
	}
	var stream upstreamStream
	if err := s.call("host.http.do_stream", payload, &stream); err != nil {
		return stream, fail(502, "upstream_transport", "Basis Points stream transport failed: "+safeError(err))
	}
	if stream.StreamID == "" {
		return stream, fail(502, "upstream_transport", "host returned no Basis Points stream ID")
	}
	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		// 非 2xx 仍有响应流；读取错误原因后关闭，避免丢失正文和泄漏流。
		raw, err := s.readUpstreamStream(stream)
		if err != nil {
			return stream, fail(stream.StatusCode, "upstream_error", "Basis Points error body could not be read: "+safeError(err))
		}
		return stream, upstreamRequestError(stream.StatusCode, raw, body, c)
	}
	return stream, nil
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return redactTokenMessage(err.Error())
}

func (s *Service) readUpstreamStream(stream upstreamStream) ([]byte, error) {
	cfg := s.config()
	if stream.StreamID == "" {
		return nil, fail(502, "upstream_transport", "upstream stream ID is empty")
	}
	defer func() { _ = s.call("host.http.stream_close", map[string]any{"stream_id": stream.StreamID}, nil) }()
	deadline := time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	var buffer bytes.Buffer
	for {
		if time.Now().After(deadline) {
			return nil, timeoutError(cfg)
		}
		var chunk streamChunk
		if err := s.call("host.http.stream_read", map[string]any{"stream_id": stream.StreamID}, &chunk); err != nil {
			return nil, fail(502, "upstream_transport", "Basis Points stream read failed: "+safeError(err))
		}
		if chunk.Error != "" {
			return nil, fail(502, "upstream_transport", "Basis Points stream interrupted: "+safeError(errors.New(chunk.Error)))
		}
		if len(chunk.Payload) > 0 {
			if buffer.Len()+len(chunk.Payload) > cfg.MaxResponseBytes {
				return nil, fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
			}
			_, _ = buffer.Write(chunk.Payload)
		}
		if chunk.Done {
			return buffer.Bytes(), nil
		}
	}
}

type sseDecoder struct {
	buffer strings.Builder
	data   []string
	event  string
}

func newSSEDecoder() *sseDecoder { return &sseDecoder{} }

func (d *sseDecoder) feed(chunk []byte, emit func(event, data string) error) error {
	d.buffer.Write(chunk)
	text := d.buffer.String()
	for {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			d.buffer.Reset()
			d.buffer.WriteString(text)
			return nil
		}
		line := strings.TrimSuffix(text[:index], "\r")
		text = text[index+1:]
		if line == "" {
			if len(d.data) > 0 {
				if err := emit(d.event, strings.Join(d.data, "\n")); err != nil {
					return err
				}
			}
			d.data = nil
			d.event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			d.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			d.data = append(d.data, strings.TrimPrefix(value, " "))
		}
	}
}

// 仅附加非敏感摘要，不记录对话正文、图片内容或认证信息。
func upstreamRequestError(status int, raw []byte, body map[string]any, c credential) error {
	redacted := string(raw)
	for _, secret := range []string{c.AccessToken, c.AccountID, c.Email} {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}
	message := redactTokenMessage(errorMessage([]byte(redacted)))
	images, originalDetails := 0, 0
	items, _ := body["input"].([]any)
	for _, value := range items {
		parts, _ := objectValue(value)["content"].([]any)
		for _, part := range parts {
			if stringValue(objectValue(part)["type"]) == "input_image" {
				images++
				if stringValue(objectValue(part)["detail"]) == "original" {
					originalDetails++
				}
			}
		}
	}
	tier := "unspecified"
	if value, exists := body["service_tier"]; exists {
		switch stringValue(value) {
		case "auto", "default", "flex", "priority", "scale":
			tier = stringValue(value)
		default:
			tier = "invalid"
		}
	}
	return fail(status, "upstream_error", fmt.Sprintf("Basis Points HTTP %d: %s (reasoning_effort=%s; service_tier=%s; input_images=%d; original_detail_images=%d)", status, message, stringValue(body["reasoning_effort"]), tier, images, originalDetails))
}
