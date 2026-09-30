package basispoints

import (
	"bytes"
	"errors"
	"fmt"
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

func (s *Service) upstreamRequest(request ExecutorRequest, body map[string]any, c credential, stream bool) (upstreamResponse, error) {
	cfg := s.config()
	if cfg.ResponsesURL == "" {
		return upstreamResponse{}, fail(500, "invalid_config", "responses_url is empty")
	}
	response, err := s.doHTTP(request, cfg.ResponsesURL, responseHeaders(c, stream), jsonBytes(body))
	if err != nil {
		return upstreamResponse{}, transportError(err, "upstream_transport", "Basis Points transport failed: ")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, upstreamRequestError(response.StatusCode, response.Body, body, c)
	}
	return response, nil
}

func (s *Service) upstreamStream(request ExecutorRequest, body map[string]any, c credential) (upstreamStream, error) {
	cfg := s.config()
	stream, err := s.openHTTPStream(request, cfg.ResponsesURL, responseHeaders(c, true), jsonBytes(body))
	if err != nil {
		return stream, transportError(err, "upstream_transport", "Basis Points stream transport failed: ")
	}
	if stream.StreamID == "" && stream.local == nil {
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
	if stream.StreamID == "" && stream.local == nil {
		return nil, fail(502, "upstream_transport", "upstream stream ID is empty")
	}
	defer s.closeHTTPStream(stream)
	deadline := time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	var buffer bytes.Buffer
	for {
		if time.Now().After(deadline) {
			return nil, timeoutError(cfg)
		}
		chunk, err := s.readHTTPStream(stream)
		if err != nil {
			return nil, transportError(err, "upstream_transport", "Basis Points stream read failed: ")
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
	// Redact the raw body before errorMessage truncates it so long credentials cannot
	// leak; redact the decoded message again for dynamic headers containing quotes or escapes.
	message := errorMessage([]byte(c.redactMessage(string(raw))))
	message = c.redactMessage(message)
	images, originalDetails := 0, 0
	var imageRefs []string
	items, _ := body["input"].([]any)
	for i, value := range items {
		parts, _ := objectValue(value)["content"].([]any)
		for j, value := range parts {
			part := objectValue(value)
			if stringValue(part["type"]) == "input_image" {
				images++
				if stringValue(part["detail"]) == "original" {
					originalDetails++
				}
				// 只输出上游请求中的索引和引用类型，不输出 URL、文件 ID 或图片内容。
				if len(imageRefs) < 16 {
					kind := "missing"
					if stringValue(part["file_id"]) != "" {
						kind = "file_id"
					} else if imageURL := stringValue(part["image_url"]); imageURL != "" {
						kind = "image_url"
						if len(imageURL) >= 5 && strings.EqualFold(imageURL[:5], "data:") {
							kind = "data_url"
						}
					}
					imageRefs = append(imageRefs, fmt.Sprintf("input[%d].content[%d]:%s", i, j, kind))
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
	imageSummary := ""
	if images > 0 {
		imageSummary = "; image_refs=" + strings.Join(imageRefs, ",")
		if images > len(imageRefs) {
			imageSummary += fmt.Sprintf(",...(%d more)", images-len(imageRefs))
		}
	}
	return fail(status, "upstream_error", fmt.Sprintf("Basis Points HTTP %d: %s (reasoning_effort=%s; service_tier=%s; input_images=%d; original_detail_images=%d%s)", status, message, stringValue(body["reasoning_effort"]), tier, images, originalDetails, imageSummary))
}
