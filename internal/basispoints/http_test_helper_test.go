package basispoints

// 协议夹具直接注入认证及宿主 HTTP 回调，显式使用 virtual；默认 host 由独立用例覆盖。
func newVirtualTestService() *Service {
	svc := NewService()
	svc.cfg.CredentialSource = CredentialSourceVirtual
	return svc
}

// 既有用例验证原 HTTP/SSE 协议；WS 默认优先及回退由独立真实 socket 测试覆盖。
func newHTTPTestService() *Service {
	svc := newVirtualTestService()
	svc.cfg.UpstreamTransport = "http"
	return svc
}
