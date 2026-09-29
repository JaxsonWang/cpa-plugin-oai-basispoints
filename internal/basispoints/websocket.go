package basispoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// selected 为真表示已选择 WS；之后即使没有收到正文，也不能转 HTTP 重放一次生成。
func (s *Service) tryWebSocket(request ExecutorRequest, body map[string]any, c credential, run *runningStream, delivery *streamDelivery) (response map[string]any, selected bool, err error) {
	cfg := s.config()
	if cfg.UpstreamTransport == "http" || !credentialWebsocketsEnabled(request) {
		return nil, false, nil
	}
	if err := run.contextError(); err != nil {
		return nil, true, err
	}
	target, err := url.Parse(cfg.ResponsesURL)
	if err != nil {
		return nil, true, fail(400, "invalid_config", "invalid upstream URL")
	}
	if target.Scheme == "https" {
		target.Scheme = "wss"
	} else {
		target.Scheme = "ws"
	}
	proxy, supported := websocketProxy(request)
	if !supported {
		s.logWebSocketFallback("proxy_not_supported", 0)
		return nil, false, nil
	}
	dialer := websocket.Dialer{HandshakeTimeout: time.Duration(cfg.WSHandshakeTimeoutSeconds) * time.Second, Proxy: proxy}
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			// 绑定原始连接，取消发生在 TLS/Upgrade 阶段也能立即中断，不必等握手超时。
			run.setClose(func() { _ = conn.Close() })
		}
		return conn, err
	}
	headers := authHeaders(c, false)
	headers.Del("Content-Type")
	conn, handshake, dialErr := dialer.DialContext(run.ctx, target.String(), headers)
	if dialErr != nil {
		run.closeUpstream()
		status := 0
		var errorBody []byte
		if handshake != nil {
			status = handshake.StatusCode
			if handshake.Body != nil {
				errorBody, _ = io.ReadAll(io.LimitReader(handshake.Body, 64<<10))
				_ = handshake.Body.Close()
			}
		}
		if err := run.contextError(); err != nil {
			return nil, true, err
		}
		// 鉴权、代理鉴权及限流不是协议不可用；保持原状态，不换通道掩盖错误。
		if status == 401 || status == 403 || status == 407 || status == 429 {
			return nil, true, upstreamRequestError(status, errorBody, body, c)
		}
		reason := "connection_failed"
		var timeout net.Error
		if errors.As(dialErr, &timeout) && timeout.Timeout() {
			reason = "handshake_timeout"
		} else if handshake != nil {
			reason = "handshake_rejected"
		}
		s.logWebSocketFallback(reason, status)
		return nil, false, nil
	}
	run.setClose(func() { _ = conn.Close() })
	defer run.closeUpstream()
	conn.SetReadLimit(int64(cfg.MaxResponseBytes))
	if deadline, ok := run.ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
		_ = conn.SetWriteDeadline(deadline)
	}
	if err := run.contextError(); err != nil {
		return nil, true, err
	}
	payload := cloneObject(body)
	payload["type"] = "response.create"
	delete(payload, "stream")
	delete(payload, "background")
	// 发送调用可能已经部分到达上游；从此处起无论返回什么错误都不重放。
	if err := conn.WriteMessage(websocket.TextMessage, jsonBytes(payload)); err != nil {
		if canceled := run.contextError(); canceled != nil {
			return nil, true, canceled
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, true, timeoutError(cfg)
		}
		return nil, true, fail(502, "upstream_ws_write", "Basis Points WebSocket request could not be sent completely; not replayed over HTTP")
	}
	var wire strings.Builder
	for {
		kind, data, readErr := conn.ReadMessage()
		if err := run.contextError(); err != nil {
			return nil, true, err
		}
		if readErr != nil {
			if errors.Is(readErr, websocket.ErrReadLimit) {
				return nil, true, fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
			}
			var timeout net.Error
			if errors.As(readErr, &timeout) && timeout.Timeout() {
				return nil, true, timeoutError(cfg)
			}
			message := "Basis Points WebSocket closed before a terminal response; not replayed over HTTP"
			var closed *websocket.CloseError
			if errors.As(readErr, &closed) {
				// 不回显上游关闭原因正文，只暴露标准关闭码供定位断流。
				message += fmt.Sprintf(" (close_code=%d)", closed.Code)
			}
			return nil, true, fail(502, "upstream_ws_interrupted", message)
		}
		if kind != websocket.TextMessage {
			return nil, true, fail(502, "invalid_upstream_response", "Basis Points WebSocket returned a non-text event")
		}
		event, reason := parseRelayObject(string(data))
		if reason != "" || stringValue(event["type"]) == "" {
			return nil, true, fail(502, "invalid_upstream_response", "Basis Points WebSocket returned invalid event JSON")
		}
		name := stringValue(event["type"])
		var frame strings.Builder
		writeSSE(&frame, name, event)
		if wire.Len()+frame.Len() > cfg.MaxResponseBytes {
			return nil, true, fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
		}
		wire.WriteString(frame.String())
		if delivery != nil {
			if err := delivery.consume(name, string(data)); err != nil {
				return nil, true, err
			}
		}
		switch name {
		case "response.completed", "response.incomplete", "response.failed", "response.cancelled", "error":
			result, err := parseResponse([]byte(wire.String()), http.Header{"Content-Type": {"text/event-stream"}})
			return result, true, err
		}
	}
}

// 采用 CPA 的属性优先级；运行时关闭覆盖旧 StorageJSON，缺失或无效值不能开启 WS。
func credentialWebsocketsEnabled(request ExecutorRequest) bool {
	if raw := strings.TrimSpace(request.AuthAttributes["websockets"]); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			return enabled
		}
	}
	value, exists := request.AuthMetadata["websockets"]
	if !exists {
		var stored map[string]any
		if json.Unmarshal(request.StorageJSON, &stored) == nil {
			value = stored["websockets"]
		}
	}
	switch flag := value.(type) {
	case bool:
		return flag
	case string:
		enabled, err := strconv.ParseBool(strings.TrimSpace(flag))
		return err == nil && enabled
	default:
		return false
	}
}

func websocketProxy(request ExecutorRequest) (func(*http.Request) (*url.URL, error), bool) {
	value := strings.TrimSpace(request.AuthAttributes["basispoints_proxy_url"])
	// 单独执行 ABI 时仍可使用凭据自身的代理；正常 CPA 路径由 auth.parse 传递有效代理。
	var storage map[string]any
	if json.Unmarshal(request.StorageJSON, &storage) == nil {
		if explicit := strings.TrimSpace(stringValue(storage["proxy_url"])); explicit != "" {
			value = explicit
		}
	}
	if value == "" {
		return http.ProxyFromEnvironment, true
	}
	if value == "direct" || value == "none" {
		return nil, true
	}
	proxy, err := url.Parse(value)
	if err != nil || proxy.Host == "" {
		return nil, false
	}
	if proxy.Scheme == "socks5h" {
		proxy.Scheme = "socks5"
	}
	if proxy.Scheme != "http" && proxy.Scheme != "socks5" {
		// 不支持的宿主代理仍交回宿主 HTTP 通道，绝不绕过代理直接发送 OAuth。
		return nil, false
	}
	return http.ProxyURL(proxy), true
}

func (s *Service) logWebSocketFallback(reason string, status int) {
	attempts := 1
	if reason == "proxy_not_supported" {
		attempts = 0
	}
	_ = s.call("host.log", map[string]any{"level": "info", "message": fmt.Sprintf("Basis Points WS fallback to HTTP: reason=%s status=%d handshake_attempts=%d", reason, status, attempts)}, nil)
}
