package basispoints

// 既有用例验证原 HTTP/SSE 协议；WS 默认优先及回退由独立真实 socket 测试覆盖。
func newHTTPTestService() *Service {
	svc := NewService()
	svc.cfg.UpstreamTransport = "http"
	return svc
}
