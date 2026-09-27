package basispoints

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"
)

// 异步读取在 RPC 返回后仍属于插件；停用时先取消上游，再等待工作协程退出。
type runningStream struct {
	service       *Service
	mu            sync.Mutex
	close         func()
	ctx           context.Context
	cancel        context.CancelFunc
	stopCloseHook func() bool
	closeHookDone chan struct{}
	canceled      bool
}

func (r *runningStream) closeUpstream() {
	r.mu.Lock()
	close := r.close
	r.close = nil
	r.mu.Unlock()
	if close != nil {
		close()
	}
}

func (r *runningStream) setClose(close func()) {
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		close()
		return
	}
	r.close = close
	r.mu.Unlock()
}

func (r *runningStream) finish() {
	if !r.stopCloseHook() {
		<-r.closeHookDone
	}
	r.cancel()
	r.closeUpstream()
	r.service.mu.Lock()
	delete(r.service.streams, r)
	r.service.mu.Unlock()
	r.service.streamWG.Done()
}

func (r *runningStream) contextError() error {
	if r.stopped() {
		return fail(503, "plugin_stopped", "plugin stopped while processing upstream response")
	}
	if errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
		return timeoutError(r.service.config())
	}
	if r.ctx.Err() != nil {
		return fail(499, "client_disconnected", "request canceled while processing upstream response")
	}
	return nil
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
	scopes := s.requests
	s.requests = nil
	s.mu.Unlock()
	for _, scope := range scopes {
		scope.cancel()
	}
	for _, r := range active {
		r.mu.Lock()
		r.canceled = true
		r.mu.Unlock()
		r.cancel()
		r.closeUpstream()
	}
	s.streamWG.Wait()
}

func (s *Service) executeStream(request ExecutorRequest, body map[string]any, c credential) (any, error) {
	if request.StreamID == "" {
		return nil, fail(500, "stream_id_missing", "executor.execute_stream requires stream_id")
	}
	run, err := s.startRun(request)
	if err != nil {
		return nil, err
	}
	ready := make(chan error, 1)
	delivery := newStreamDelivery(request.Format, func() { ready <- nil }, func(frame []byte) error {
		return s.call("host.stream.emit", map[string]any{"stream_id": request.StreamID, "payload": frame}, nil)
	})
	go func() {
		defer run.finish()
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
	if err := run.contextError(); err != nil {
		return nil, err
	}
	if response, selected, err := s.tryWebSocket(request, body, c, run, delivery); selected || err != nil {
		return response, err
	}
	if err := run.contextError(); err != nil {
		return nil, err
	}
	upstream, err := s.upstreamStream(request, body, c)
	if err != nil {
		return nil, err
	}
	run.setClose(func() { _ = s.call("host.http.stream_close", map[string]any{"stream_id": upstream.StreamID}, nil) })
	defer run.closeUpstream()
	cfg := s.config()
	deadline := time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	var raw bytes.Buffer
	decoder := newSSEDecoder()
	var jsonBody bool
	var detected bool
	consume := func(event, data string) error { return delivery.consume(event, data) }
	for {
		if err := run.contextError(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, timeoutError(cfg)
		}
		var chunk streamChunk
		if err := s.call("host.http.stream_read", map[string]any{"stream_id": upstream.StreamID}, &chunk); err != nil {
			return nil, fail(502, "upstream_transport", "Basis Points stream read failed: "+safeError(err))
		}
		if err := run.contextError(); err != nil {
			return nil, err
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
