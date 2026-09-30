package basispoints

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// self 路由没有宿主选中的认证，host.http.* 的 ABI 也不能重新绑定账号代理。
// host 模式因此使用绑定本次凭据的传输；不是宿主请求失败后的重试或直连回退。
type credentialHTTPStream struct {
	body io.ReadCloser
	run  *runningStream
	once sync.Once
}

type credentialHTTPClient struct {
	proxy  string
	client *http.Client
}

func (s *credentialHTTPStream) close() {
	s.once.Do(s.run.finish)
}

func credentialHTTPProxy(value string) (func(*http.Request) (*url.URL, error), error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return http.ProxyFromEnvironment, nil
	}
	if value == "direct" || value == "none" {
		return nil, nil
	}
	proxy, err := url.Parse(value)
	if err != nil || proxy.Hostname() == "" {
		return nil, fail(400, "invalid_proxy", "CPA credential proxy URL is invalid")
	}
	switch proxy.Scheme {
	case "http", "https", "socks5", "socks5h":
		return http.ProxyURL(proxy), nil
	default:
		return nil, fail(400, "invalid_proxy", "CPA credential proxy scheme is unsupported")
	}
}

func (s *Service) credentialClient(request ExecutorRequest) (*http.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, fail(503, "plugin_stopped", "oai-basispoints is shut down")
	}
	if cached, exists := s.credentialClients[request.AuthID]; exists && cached.proxy == *request.credentialProxy {
		return cached.client, nil
	}
	proxy, err := credentialHTTPProxy(*request.credentialProxy)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = proxy
	transport.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
		if response.StatusCode != http.StatusOK {
			return fail(response.StatusCode, "upstream_proxy", "CPA credential proxy rejected the connection")
		}
		return nil
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		// 上游重定向不允许自动重放带 OAuth 的 POST。
		return http.ErrUseLastResponse
	}}
	if s.credentialClients == nil {
		s.credentialClients = make(map[string]credentialHTTPClient)
	}
	if previous, exists := s.credentialClients[request.AuthID]; exists {
		previous.client.CloseIdleConnections()
	}
	s.credentialClients[request.AuthID] = credentialHTTPClient{proxy: *request.credentialProxy, client: client}
	return client, nil
}

func (s *Service) openCredentialHTTP(request ExecutorRequest, endpoint string, headers http.Header, body []byte) (upstreamStream, error) {
	client, err := s.credentialClient(request)
	if err != nil {
		return upstreamStream{}, err
	}
	run, err := s.startRun(request)
	if err != nil {
		return upstreamStream{}, err
	}
	httpRequest, err := http.NewRequestWithContext(run.ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err == nil {
		httpRequest.Header = headers.Clone()
		var response *http.Response
		response, err = client.Do(httpRequest)
		if err == nil {
			run.setClose(func() {
				_ = response.Body.Close()
			})
			return upstreamStream{
				StatusCode: response.StatusCode, Headers: response.Header,
				local: &credentialHTTPStream{body: response.Body, run: run},
			}, nil
		}
	}
	contextErr := run.contextError()
	run.finish()
	if contextErr != nil {
		return upstreamStream{}, contextErr
	}
	var apiError *APIError
	if errors.As(err, &apiError) {
		return upstreamStream{}, apiError
	}
	// net/http 错误可能包含代理 URL 和用户信息，不回显整个错误链。
	return upstreamStream{}, fail(502, "upstream_transport", "Basis Points credential transport failed")
}

func httpRequestPayload(request ExecutorRequest, endpoint string, headers http.Header, body []byte) map[string]any {
	return map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost, "url": endpoint, "headers": headers, "body": body,
	}
}

func (s *Service) doHTTP(request ExecutorRequest, endpoint string, headers http.Header, body []byte) (upstreamResponse, error) {
	if request.credentialProxy == nil {
		var response upstreamResponse
		err := s.call("host.http.do", httpRequestPayload(request, endpoint, headers, body), &response)
		return response, err
	}
	stream, err := s.openCredentialHTTP(request, endpoint, headers, body)
	if err != nil {
		return upstreamResponse{}, err
	}
	raw, err := s.readUpstreamStream(stream)
	return upstreamResponse{StatusCode: stream.StatusCode, Headers: stream.Headers, Body: raw}, err
}

func (s *Service) openHTTPStream(request ExecutorRequest, endpoint string, headers http.Header, body []byte) (upstreamStream, error) {
	if request.credentialProxy != nil {
		return s.openCredentialHTTP(request, endpoint, headers, body)
	}
	var stream upstreamStream
	err := s.call("host.http.do_stream", httpRequestPayload(request, endpoint, headers, body), &stream)
	return stream, err
}

func (s *Service) closeHTTPStream(stream upstreamStream) {
	if stream.local != nil {
		stream.local.close()
		return
	}
	_ = s.call("host.http.stream_close", map[string]any{"stream_id": stream.StreamID}, nil)
}

func (s *Service) readHTTPStream(stream upstreamStream) (streamChunk, error) {
	if stream.local == nil {
		var chunk streamChunk
		err := s.call("host.http.stream_read", map[string]any{"stream_id": stream.StreamID}, &chunk)
		return chunk, err
	}
	buffer := make([]byte, 32*1024)
	n, err := stream.local.body.Read(buffer)
	if contextErr := stream.local.run.contextError(); contextErr != nil {
		return streamChunk{}, contextErr
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return streamChunk{}, fail(502, "upstream_transport", "Basis Points credential response was interrupted")
	}
	return streamChunk{Payload: buffer[:n], Done: errors.Is(err, io.EOF)}, nil
}

func transportError(err error, kind, message string) error {
	var apiError *APIError
	if errors.As(err, &apiError) {
		return apiError
	}
	return fail(502, kind, message+safeError(err))
}
