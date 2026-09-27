package basispoints

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const requestLifecycleHeader = "X-Oai-Basispoints-Request-Id"

type requestScope struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// 直接 WS 连接仍跟随 CPA 的原生请求生命周期；不通过轮询或额外网络请求感知取消。
func (s *Service) interceptUpstreamRequest(raw json.RawMessage) (any, error) {
	var request struct {
		RequestID      string
		Model          string
		RequestedModel string
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fail(400, "invalid_request", "invalid request lifecycle payload")
	}
	cfg := s.config()
	model := request.RequestedModel
	if model == "" {
		model = request.Model
	}
	enabled := false
	for _, alias := range cfg.Models {
		enabled = enabled || model == alias
	}
	if !enabled {
		return map[string]any{}, nil
	}
	if cfg.UpstreamTransport == "http" {
		return map[string]any{"ClearHeaders": []string{requestLifecycleHeader}}, nil
	}
	if request.RequestID == "" {
		return nil, fail(400, "request_id_missing", "host did not provide a request lifecycle ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fail(503, "plugin_stopped", "oai-basispoints is shut down")
	}
	if s.requests == nil {
		s.requests = make(map[string]*requestScope)
	}
	if s.requests[request.RequestID] == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.requests[request.RequestID] = &requestScope{ctx: ctx, cancel: cancel}
	}
	return map[string]any{"Headers": http.Header{requestLifecycleHeader: {request.RequestID}}}, nil
}

func (s *Service) completeUpstreamRequest(raw json.RawMessage) (any, error) {
	var completion struct{ RequestID string }
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, fail(400, "invalid_request", "invalid request completion payload")
	}
	s.mu.Lock()
	scope := s.requests[completion.RequestID]
	delete(s.requests, completion.RequestID)
	s.mu.Unlock()
	if scope != nil {
		scope.cancel()
	}
	return map[string]any{}, nil
}

func (s *Service) startRun(request ExecutorRequest) (*runningStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fail(503, "plugin_stopped", "oai-basispoints is shut down")
	}
	parent := context.Background()
	if id := request.Headers.Get(requestLifecycleHeader); id != "" {
		scope := s.requests[id]
		if scope == nil {
			return nil, fail(499, "client_disconnected", "request already completed")
		}
		parent = scope.ctx
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(s.cfg.TimeoutSeconds)*time.Second)
	run := &runningStream{service: s, ctx: ctx, cancel: cancel, closeHookDone: make(chan struct{})}
	if s.streams == nil {
		s.streams = make(map[*runningStream]struct{})
	}
	s.streams[run] = struct{}{}
	s.streamWG.Add(1)
	run.stopCloseHook = context.AfterFunc(ctx, func() {
		run.closeUpstream()
		close(run.closeHookDone)
	})
	return run, nil
}
