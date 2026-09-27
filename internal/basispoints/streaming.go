package basispoints

import (
	"bytes"
	"errors"
	"sync"
	"time"
)

// 异步读取在 RPC 返回后仍属于插件；停用时先取消上游，再等待工作协程退出。
type runningStream struct {
	service  *Service
	mu       sync.Mutex
	upstream string
	canceled bool
}

func (r *runningStream) closeUpstream() {
	r.mu.Lock()
	id := r.upstream
	r.upstream = ""
	r.mu.Unlock()
	if id != "" {
		_ = r.service.call("host.http.stream_close", map[string]any{"stream_id": id}, nil)
	}
}

func (r *runningStream) stopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.canceled
}

func (s *Service) stopStreams() {
	s.mu.Lock()
	s.stopped = true
	active := make([]*runningStream, 0, len(s.streams))
	for r := range s.streams {
		active = append(active, r)
	}
	s.mu.Unlock()
	for _, r := range active {
		r.mu.Lock()
		r.canceled = true
		r.mu.Unlock()
		r.closeUpstream()
	}
	s.streamWG.Wait()
}

func (s *Service) executeStream(request ExecutorRequest, body map[string]any, c credential) (any, error) {
	if request.StreamID == "" {
		return nil, fail(500, "stream_id_missing", "executor.execute_stream requires stream_id")
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, fail(503, "plugin_stopped", "oai-basispoints is shut down")
	}
	run := &runningStream{service: s}
	if s.streams == nil {
		s.streams = make(map[*runningStream]struct{})
	}
	s.streams[run] = struct{}{}
	s.streamWG.Add(1)
	s.mu.Unlock()
	ready := make(chan error, 1)
	delivery := newStreamDelivery(request.Format, func() { ready <- nil }, func(frame []byte) error {
		return s.call("host.stream.emit", map[string]any{"stream_id": request.StreamID, "payload": frame}, nil)
	})
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.streams, run)
			s.mu.Unlock()
			s.streamWG.Done()
		}()
		response, err := s.readStreamingResponse(request, body, c, run, delivery)
		if err == nil {
			err = delivery.finish(response)
		}
		if !delivery.committed {
			ready <- err
			return
		}
		if err != nil && !delivery.disconnected {
			_ = delivery.fail(err)
		}
		// 业务失败已通过协议 error 事件交付，不能让 ABI 的无类型错误误变为 500/凭据冷却。
		// 此处只是关闭传输，不发送 completed，也不把失败伪装成成功响应。
		_ = s.call("host.stream.close", map[string]any{"stream_id": request.StreamID}, nil)
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	return map[string]any{"Headers": map[string][]string{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}}, nil
}

func (s *Service) readStreamingResponse(request ExecutorRequest, body map[string]any, c credential, run *runningStream, delivery *streamDelivery) (map[string]any, error) {
	source, err := executorSource(request)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		response, err := s.readStreamAttempt(request, body, c, run, delivery)
		if err != nil {
			return nil, err
		}
		if err = delivery.validateFinal(response); err != nil {
			return nil, err
		}
		_, transformed, _, err := transformResponseBody(jsonBytes(response), source)
		if err == nil {
			return transformed, nil
		}
		var apiError *APIError
		if delivery.committed || attempt != 0 || response["status"] == "incomplete" || !errors.As(err, &apiError) || apiError.Kind != "invalid_tool_call" {
			return nil, err
		}
		// 没有提交任何客户端数据的工具请求仍保留原有一次重生成；已经输出则绝不重跑。
		retry := cloneObject(body)
		items, _ := body["input"].([]any)
		retry["input"] = appendBeforeCompaction(append([]any{}, items...), []any{messageItem("developer", transportRetryHint+" Diagnostic: "+apiError.Message)})
		body = retry
		*delivery = *newStreamDelivery(delivery.format, delivery.start, delivery.write)
	}
	return nil, relayError("retry_exhausted")
}

func (s *Service) readStreamAttempt(request ExecutorRequest, body map[string]any, c credential, run *runningStream, delivery *streamDelivery) (map[string]any, error) {
	if run.stopped() {
		return nil, fail(503, "plugin_stopped", "plugin stopped while streaming")
	}
	upstream, err := s.upstreamStream(request, body, c)
	if err != nil {
		return nil, err
	}
	run.mu.Lock()
	run.upstream = upstream.StreamID
	run.mu.Unlock()
	defer run.closeUpstream()
	cfg := s.config()
	deadline := time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	var raw bytes.Buffer
	decoder := newSSEDecoder()
	var jsonBody bool
	var detected bool
	consume := func(event, data string) error { return delivery.consume(event, data) }
	for {
		if run.stopped() {
			return nil, fail(503, "plugin_stopped", "plugin stopped while streaming")
		}
		if time.Now().After(deadline) {
			return nil, timeoutError(cfg)
		}
		var chunk streamChunk
		if err := s.call("host.http.stream_read", map[string]any{"stream_id": upstream.StreamID}, &chunk); err != nil {
			return nil, fail(502, "upstream_transport", "Basis Points stream read failed: "+safeError(err))
		}
		if run.stopped() {
			return nil, fail(503, "plugin_stopped", "plugin stopped while streaming")
		}
		if time.Now().After(deadline) {
			return nil, timeoutError(cfg)
		}
		if chunk.Error != "" {
			return nil, fail(502, "upstream_transport", "Basis Points stream interrupted: "+safeError(errors.New(chunk.Error)))
		}
		if raw.Len()+len(chunk.Payload) > cfg.MaxResponseBytes {
			return nil, fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
		}
		_, _ = raw.Write(chunk.Payload)
		feed := chunk.Payload
		if !detected {
			if trimmed := bytes.TrimSpace(raw.Bytes()); len(trimmed) > 0 {
				detected, jsonBody = true, trimmed[0] == '{'
				feed = raw.Bytes()
			}
		}
		if detected && !jsonBody {
			if err := decoder.feed(feed, consume); err != nil {
				return nil, err
			}
		}
		if chunk.Done {
			if detected && !jsonBody {
				if err := decoder.feed([]byte{10, 10}, consume); err != nil {
					return nil, err
				}
			}
			return parseResponse(raw.Bytes(), upstream.Headers)
		}
	}
}
