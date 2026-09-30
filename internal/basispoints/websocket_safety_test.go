package basispoints

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type websocketCapture struct {
	mu        sync.Mutex
	frames    [][]byte
	fallbacks atomic.Int32
	closed    chan struct{}
}

func (c *websocketCapture) host(method string, payload any, out any) error {
	switch method {
	case "host.log":
	case "host.http.do":
		c.fallbacks.Add(1)
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(incrementalTerminal("HTTP_OK"))}
	case "host.http.do_stream":
		c.fallbacks.Add(1)
		*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "fallback"}
	case "host.http.stream_read":
		*out.(*streamChunk) = streamChunk{Payload: syntheticStream(incrementalTerminal("HTTP_OK")), Done: true}
	case "host.http.stream_close":
	case "host.stream.emit":
		c.mu.Lock()
		c.frames = append(c.frames, bytes.Clone(payload.(map[string]any)["payload"].([]byte)))
		c.mu.Unlock()
	case "host.stream.close":
		c.closed <- struct{}{}
	default:
		return fmt.Errorf("unexpected callback %s", method)
	}
	return nil
}

func websocketRequest(stream bool) ExecutorRequest {
	return ExecutorRequest{Model: DefaultModelID, Format: "openai-response", Stream: stream, StreamID: "client", Payload: jsonBytes(map[string]any{"model": DefaultModelID, "input": "hello"}), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture-account"}, AuthAttributes: map[string]string{"websockets": "true"}}
}

func writeWebSocketEvents(conn *websocket.Conn, wire []byte) error {
	return newSSEDecoder().feed(wire, func(_ string, data string) error {
		if data == "[DONE]" {
			return nil
		}
		return conn.WriteMessage(websocket.TextMessage, []byte(data))
	})
}

func TestWebSocketSubmittedFailuresNeverFallBack(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, earlyText := range []bool{false, true} {
			for _, mode := range []string{"closed", "bad_json", "binary", "upstream_failed", "size_limit"} {
				t.Run(fmt.Sprintf("stream=%t/text=%t/%s", stream, earlyText, mode), func(t *testing.T) {
					var creates atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						var request map[string]any
						if conn.ReadJSON(&request) != nil {
							return
						}
						creates.Add(1)
						if earlyText {
							_ = writeWebSocketEvents(conn, incrementalPrefix("  你好\r\n"))
						}
						switch mode {
						case "bad_json":
							_ = conn.WriteMessage(websocket.TextMessage, []byte("{bad-json}"))
						case "binary":
							_ = conn.WriteMessage(websocket.BinaryMessage, []byte("not-a-text-event"))
						case "upstream_failed":
							_ = conn.WriteJSON(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed"}})
						case "size_limit":
							_ = conn.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte("x"), 4096))
						}
					}))
					defer server.Close()
					svc := newVirtualTestService()
					svc.cfg.ResponsesURL = server.URL
					if mode == "size_limit" {
						svc.cfg.MaxResponseBytes = 3000
					}
					capture := &websocketCapture{closed: make(chan struct{}, 1)}
					svc.SetHost(capture.host)
					method := "executor.execute"
					if stream {
						method = "executor.execute_stream"
					}
					_, err := svc.Handle(method, jsonBytes(websocketRequest(stream)))
					if stream && earlyText {
						if err != nil {
							t.Fatal(err)
						}
						select {
						case <-capture.closed:
						case <-time.After(5 * time.Second):
							t.Fatal("stream leaked")
						}
						capture.mu.Lock()
						events := decodeIncrementalFrames(t, "openai-response", capture.frames)
						capture.mu.Unlock()
						failures, terminals := 0, 0
						text := ""
						for _, e := range events {
							if e["type"] == "error" {
								failures++
							}
							if e["type"] == "response.completed" {
								terminals++
							}
							if e["type"] == "response.output_text.delta" {
								text += e["delta"].(string)
							}
						}
						if failures != 1 || terminals != 0 || text != "  你好\r\n" {
							t.Fatalf("failures=%d terminals=%d text=%q", failures, terminals, text)
						}
					} else if err == nil {
						t.Fatal("failure became success")
					}
					svc.streamWG.Wait()
					if capture.fallbacks.Load() != 0 || creates.Load() != 1 {
						t.Fatalf("submitted request replayed: creates=%d HTTP=%d", creates.Load(), capture.fallbacks.Load())
					}
				})
			}
		}
	}
}

func TestWebSocketLifecycleCancelsHandshakeAndPendingResponse(t *testing.T) {
	for _, stage := range []string{"handshake", "before_text", "after_text", "quiesce"} {
		t.Run(stage, func(t *testing.T) {
			ready, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stage == "handshake" {
					close(ready)
					<-release
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				if stage == "after_text" || stage == "quiesce" {
					_ = writeWebSocketEvents(conn, incrementalPrefix("hello"))
				}
				close(ready)
				<-release
			}))
			defer server.Close()
			defer close(release)
			svc := newVirtualTestService()
			svc.cfg.ResponsesURL = server.URL
			capture := &websocketCapture{closed: make(chan struct{}, 1)}
			svc.SetHost(capture.host)
			intercepted, err := svc.Handle("request.intercept_after", jsonBytes(map[string]any{"RequestID": "scoped", "RequestedModel": DefaultModelID}))
			if err != nil {
				t.Fatal(err)
			}
			request := websocketRequest(true)
			request.Headers = intercepted.(map[string]any)["Headers"].(http.Header)
			returned := make(chan error, 1)
			go func() { _, err := svc.Handle("executor.execute_stream", jsonBytes(request)); returned <- err }()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream did not start")
			}
			if stage == "after_text" || stage == "quiesce" {
				if err := <-returned; err != nil {
					t.Fatal(err)
				}
			}
			canceled := make(chan error, 1)
			go func() {
				method, raw := "request.complete", jsonBytes(map[string]any{"RequestID": "scoped", "Outcome": "canceled"})
				if stage == "quiesce" {
					method, raw = "plugin.quiesce", nil
				}
				_, err := svc.Handle(method, raw)
				canceled <- err
			}()
			select {
			case err := <-canceled:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation blocked")
			}
			if stage == "handshake" || stage == "before_text" {
				select {
				case err := <-returned:
					if api, ok := err.(*APIError); !ok || api.Status != 499 {
						t.Fatalf("cancellation status: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("WS did not promptly cancel")
				}
			} else {
				select {
				case <-capture.closed:
				case <-time.After(time.Second):
					t.Fatal("stream did not close")
				}
			}
			svc.streamWG.Wait()
			if capture.fallbacks.Load() != 0 || len(svc.requests) != 0 {
				t.Fatal("canceled request fell back or retained its lifecycle scope")
			}
		})
	}
}

func TestWebSocketHandshakeTimeoutFallsBackOnce(t *testing.T) {
	var attempts atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		<-release
	}))
	defer server.Close()
	defer close(release)
	svc := newVirtualTestService()
	svc.cfg.ResponsesURL, svc.cfg.WSHandshakeTimeoutSeconds = server.URL, 1
	capture := &websocketCapture{closed: make(chan struct{}, 1)}
	svc.SetHost(capture.host)
	started := time.Now()
	result, err := svc.Handle("executor.execute", jsonBytes(websocketRequest(false)))
	if err != nil || !bytes.Contains(result.(map[string]any)["Payload"].([]byte), []byte("HTTP_OK")) {
		t.Fatalf("timeout fallback failed: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("unexpected handshake timeout duration: %s", elapsed)
	}
	if attempts.Load() != 1 || capture.fallbacks.Load() != 1 {
		t.Fatal("handshake was retried before HTTP fallback")
	}
}

func TestWebSocketGenerationTimeoutDoesNotFallBack(t *testing.T) {
	release := make(chan struct{})
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err == nil {
			creates.Add(1)
		}
		<-release
	}))
	defer server.Close()
	defer close(release)
	svc := newVirtualTestService()
	svc.cfg.ResponsesURL, svc.cfg.TimeoutSeconds = server.URL, 1
	capture := &websocketCapture{closed: make(chan struct{}, 1)}
	svc.SetHost(capture.host)
	_, err := svc.Handle("executor.execute", jsonBytes(websocketRequest(false)))
	if api, ok := err.(*APIError); !ok || api.Status != 504 || api.Kind != "upstream_timeout" {
		t.Fatalf("generation timeout misclassified: %v", err)
	}
	if creates.Load() != 1 || capture.fallbacks.Load() != 0 {
		t.Fatal("timed out generation was replayed")
	}
}

func TestWebSocketApplicationErrorsPreserveStatusWithoutReplay(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422, 429} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", status, stream), func(t *testing.T) {
				var creates atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					creates.Add(1)
					_ = conn.WriteJSON(map[string]any{"type": "error", "status": status, "error": map[string]any{"message": "upstream rejected this request"}})
				}))
				defer server.Close()
				svc := newVirtualTestService()
				svc.cfg.ResponsesURL = server.URL
				capture := &websocketCapture{closed: make(chan struct{}, 1)}
				svc.SetHost(capture.host)
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				_, err := svc.Handle(method, jsonBytes(websocketRequest(stream)))
				if api, ok := err.(*APIError); !ok || api.Status != status {
					t.Fatalf("lost upstream error status: %v", err)
				}
				svc.streamWG.Wait()
				if creates.Load() != 1 || capture.fallbacks.Load() != 0 {
					t.Fatal("upstream rejection was replayed")
				}
			})
		}
	}
}

func TestWebSocketFailureEventsPreserveClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		event  map[string]any
		status int
		code   string
	}{
		{"request", map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"code": "context_length_exceeded", "message": "private-upstream-message"}}}, 400, "context_length_exceeded"},
		{"rate_limit", map[string]any{"type": "error", "status": 429, "error": map[string]any{"type": "rate_limit_error", "code": "rate_limit_exceeded", "message": "private-upstream-message"}}, 429, "rate_limit_exceeded"},
	} {
		for _, stream := range []bool{false, true} {
			for _, earlyText := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/early=%t", tc.name, stream, earlyText), func(t *testing.T) {
					var creates atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
						creates.Add(1)
						if earlyText {
							if err := writeWebSocketEvents(conn, incrementalPrefix("prefix")); err != nil {
								t.Error(err)
								return
							}
						}
						if err := conn.WriteJSON(tc.event); err != nil {
							t.Error(err)
						}
					}))
					defer server.Close()
					svc := newVirtualTestService()
					svc.cfg.ResponsesURL = server.URL
					capture := &websocketCapture{closed: make(chan struct{}, 1)}
					svc.SetHost(capture.host)
					method := "executor.execute"
					if stream {
						method = "executor.execute_stream"
					}
					_, err := svc.Handle(method, jsonBytes(websocketRequest(stream)))
					svc.streamWG.Wait()
					if !stream || !earlyText {
						api, ok := err.(*APIError)
						if !ok || api.Status != tc.status || api.Kind != tc.code || strings.Contains(api.Error(), "private-") {
							t.Fatalf("incorrect failure before delivery: %#v", err)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						capture.mu.Lock()
						events := decodeIncrementalFrames(t, "openai-response", capture.frames)
						capture.mu.Unlock()
						failures := 0
						for _, event := range events {
							if event["type"] == "response.completed" || event["type"] == "response.incomplete" || strings.Contains(string(jsonBytes(event)), "private-") {
								t.Fatalf("failure leaked private data or a terminal response: %v", event)
							}
							if event["type"] == "error" {
								failures++
								if event["status"] != float64(tc.status) || objectValue(event["error"])["code"] != tc.code {
									t.Fatalf("classification lost after delivery: %v", event)
								}
							}
						}
						if failures != 1 {
							t.Fatalf("error events=%d, want 1", failures)
						}
					}
					if creates.Load() != 1 || capture.fallbacks.Load() != 0 {
						t.Fatal("failed request was replayed")
					}
				})
			}
		}
	}
}

func TestWebSocketToolRegenerationRemainsBounded(t *testing.T) {
	for _, earlyText := range []bool{false, true} {
		t.Run(fmt.Sprint(earlyText), func(t *testing.T) {
			var creates atomic.Int32
			bad, _ := relayFixture(t.Name(), true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				var request map[string]any
				if conn.ReadJSON(&request) != nil {
					return
				}
				count := creates.Add(1)
				if count == 2 && !strings.Contains(string(jsonBytes(request)), "Diagnostic:") {
					t.Error("missing original bounded-regeneration hint")
				}
				if earlyText {
					_ = writeWebSocketEvents(conn, incrementalPrefix("hello"))
				}
				_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": incrementalTerminal("hello", bad)})
			}))
			defer server.Close()
			svc := newVirtualTestService()
			svc.cfg.ResponsesURL = server.URL
			capture := &websocketCapture{closed: make(chan struct{}, 1)}
			svc.SetHost(capture.host)
			request := websocketRequest(true)
			request.Payload = jsonBytes(map[string]any{"model": DefaultModelID, "input": "hello", "tools": []any{map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}}}})
			_, err := svc.Handle("executor.execute_stream", jsonBytes(request))
			if earlyText {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-capture.closed:
				case <-time.After(5 * time.Second):
					t.Fatal("stream did not close")
				}
			} else if api, ok := err.(*APIError); !ok || api.Status != 422 {
				t.Fatalf("expected bounded invalid-tool failure: %v", err)
			}
			svc.streamWG.Wait()
			want := int32(2)
			if earlyText {
				want = 1
			}
			if creates.Load() != want || capture.fallbacks.Load() != 0 {
				t.Fatalf("unexpected retries: creates=%d fallback=%d", creates.Load(), capture.fallbacks.Load())
			}
		})
	}
}

func TestWebSocketProxyInheritanceAndConfiguration(t *testing.T) {
	for _, explicit := range []string{"", "direct", "http://fixture:password@127.0.0.1:8999"} {
		raw := jsonBytes(map[string]any{"type": "codex", "access_token": "fixture", "account_id": "fixture", "proxy_url": explicit})
		parsed, err := authParse(jsonBytes(map[string]any{"Provider": "codex", "FileName": "fixture.json", "RawJSON": raw, "Host": map[string]any{"ProxyURL": "socks5h://127.0.0.1:8998"}}))
		if err != nil {
			t.Fatal(err)
		}
		virtual := parsed["Auths"].([]any)[1].(map[string]any)
		attributes := virtual["Attributes"].(map[string]string)
		want := explicit
		if want == "" {
			want = "socks5h://127.0.0.1:8998"
		}
		if attributes["basispoints_proxy_url"] != want {
			t.Fatal("credential or host proxy was lost")
		}
		proxy, supported := websocketProxy(ExecutorRequest{AuthAttributes: attributes, StorageJSON: raw})
		if !supported || (explicit == "direct" && proxy != nil) {
			t.Fatal("proxy inheritance failed")
		}
		if proxy != nil {
			u, err := proxy(&http.Request{})
			if err != nil || u == nil || (explicit == "" && u.Scheme != "socks5") {
				t.Fatal("proxy normalization failed")
			}
		}
	}
	if _, ok := websocketProxy(ExecutorRequest{AuthAttributes: map[string]string{"basispoints_proxy_url": "https://127.0.0.1:8999"}}); ok {
		t.Fatal("unsupported proxy must use host HTTP, never direct WS")
	}
	if NewService().cfg.UpstreamTransport != "auto" || NewService().cfg.WSHandshakeTimeoutSeconds != 5 {
		t.Fatal("unexpected transport defaults")
	}
	for _, mode := range []string{"auto", "http", "unknown"} {
		cfg := defaultConfig()
		cfg.UpstreamTransport = mode
		if err := cfg.normalize(); (err != nil) != (mode == "unknown") {
			t.Fatalf("invalid mode validation: %s %v", mode, err)
		}
	}
	for _, seconds := range []int{0, 1, 30, 31} {
		cfg := defaultConfig()
		cfg.WSHandshakeTimeoutSeconds = seconds
		if err := cfg.normalize(); (err != nil) != (seconds == 0 || seconds == 31) {
			t.Fatalf("invalid timeout validation: %d %v", seconds, err)
		}
	}
}

func TestWebSocketProxySurvivesCredentialRefresh(t *testing.T) {
	for _, explicit := range []string{"", "direct", "http://credential-proxy:8997"} {
		for _, host := range []any{nil, map[string]any{"ProxyURL": "socks5://host-proxy:8998"}, map[string]any{"ProxyURL": ""}} {
			request := map[string]any{
				"AuthID": "fixture", "StorageJSON": jsonBytes(map[string]any{"access_token": "fixture", "account_id": "fixture", "proxy_url": explicit}),
				"Attributes": map[string]string{"basispoints_proxy_url": "http://previous-proxy:8999"}, "Host": host,
			}
			result, err := authRefresh(jsonBytes(request))
			if err != nil {
				t.Fatal(err)
			}
			want := explicit
			if want == "" {
				want = "http://previous-proxy:8999"
				if host != nil {
					want = host.(map[string]any)["ProxyURL"].(string)
				}
			}
			auth := result["Auth"].(map[string]any)
			if auth["Attributes"].(map[string]string)["basispoints_proxy_url"] != want {
				t.Fatal("refresh changed or dropped the effective proxy")
			}
		}
	}
}
