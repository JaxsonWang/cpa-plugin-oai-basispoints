package basispoints

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func credentialTestProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	count := new(atomic.Int32)
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Method == http.MethodConnect {
			upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
			if err != nil {
				http.Error(w, "fixture connect failed", 502)
				return
			}
			defer upstream.Close()
			client, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer client.Close()
			_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			done := make(chan struct{})
			go func() {
				_, _ = io.Copy(upstream, buffered)
				_ = upstream.Close()
				close(done)
			}()
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
			<-done
			return
		}
		forward := r.Clone(r.Context())
		forward.RequestURI = ""
		response, err := transport.RoundTrip(forward)
		if err != nil {
			http.Error(w, "fixture forwarding failed", 502)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			w.Header()[key] = values
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(server.Close)
	return server, count
}

func credentialTransportService(t *testing.T, endpoint, globalProxy, accountProxy string, ws bool) (*Service, *websocketCapture) {
	t.Helper()
	svc := hostModeService(t)
	svc.cfg.ResponsesURL = endpoint
	svc.cfg.UpstreamTransport = "auto"
	_, err := svc.Handle("model.static", jsonBytes(map[string]any{"Host": map[string]any{"ProxyURL": globalProxy}}))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &hostCredentialFixture{
		entries: []hostAuthEntry{{Index: "native", Name: "native.json", Provider: AuthProviderID, Path: "/fixture/native.json"}},
		files: map[string]string{"native": string(jsonBytes(map[string]any{
			"type": "codex", "access_token": "fixture-token", "account_id": "fixture-account",
			"websockets": ws, "proxy_url": accountProxy,
		}))},
	}
	capture := &websocketCapture{closed: make(chan struct{}, 1)}
	svc.SetHost(func(method string, payload any, out any) error {
		if strings.HasPrefix(method, "host.auth.") {
			return fixture.host(method, payload, out)
		}
		if strings.HasPrefix(method, "host.http.") {
			t.Errorf("host mode used unbound HTTP callback %s", method)
			return fmt.Errorf("unexpected unbound HTTP callback")
		}
		return capture.host(method, payload, out)
	})
	t.Cleanup(svc.stopStreams)
	return svc, capture
}

func TestHostCredentialProxyAcrossTransports(t *testing.T) {
	for _, mode := range []string{"http", "sse", "ws", "ws-fallback"} {
		for _, source := range []string{"global", "credential", "direct"} {
			t.Run(mode+"/"+source, func(t *testing.T) {
				var received atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
						t.Error("selected credential was not used")
					}
					if websocket.IsWebSocketUpgrade(r) {
						if mode == "ws-fallback" {
							http.Error(w, "fixture has no websocket endpoint", 404)
							return
						}
						conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						var body map[string]any
						if err := conn.ReadJSON(&body); err != nil {
							t.Error(err)
							return
						}
						received.Add(1)
						_ = writeWebSocketEvents(conn, syntheticStream(incrementalTerminal("PROXY_OK")))
						return
					}
					received.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(syntheticStream(incrementalTerminal("PROXY_OK")))
				}))
				defer server.Close()
				global, globalCount := credentialTestProxy(t)
				account, accountCount := credentialTestProxy(t)
				accountProxy := ""
				if source == "credential" {
					accountProxy = account.URL
				} else if source == "direct" {
					accountProxy = "direct"
				}
				svc, capture := credentialTransportService(t, server.URL, global.URL, accountProxy, strings.HasPrefix(mode, "ws"))
				stream := mode == "sse"
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				_, err := svc.Handle(method, jsonBytes(websocketRequest(stream)))
				if err != nil {
					t.Fatal(err)
				}
				if stream {
					select {
					case <-capture.closed:
					case <-time.After(3 * time.Second):
						t.Fatal("stream did not close")
					}
				}
				want := int32(1)
				if mode == "ws-fallback" {
					want = 2
				}
				globalWant, accountWant := int32(0), int32(0)
				if source == "global" {
					globalWant = want
				}
				if source == "credential" {
					accountWant = want
				}
				if received.Load() != 1 || globalCount.Load() != globalWant || accountCount.Load() != accountWant {
					t.Fatalf("generation/global/credential counts=%d/%d/%d, want=1/%d/%d", received.Load(), globalCount.Load(), accountCount.Load(), globalWant, accountWant)
				}
			})
		}
	}
}

func TestHostCredentialAttachmentUsesSameProxy(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/attachments" {
			_, _ = w.Write([]byte(`{"openai_file_id":"fixture-file"}`))
			return
		}
		_, _ = w.Write(jsonBytes(incrementalTerminal("IMAGE_OK")))
	}))
	defer server.Close()
	proxy, count := credentialTestProxy(t)
	svc, _ := credentialTransportService(t, server.URL+"/responses", "", proxy.URL, false)
	image, _ := testImageDataURL(t)
	request := websocketRequest(false)
	request.Payload = jsonBytes(map[string]any{"model": DefaultModelID, "input": []any{map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": image}},
	}}})
	if _, err := svc.Handle("executor.execute", jsonBytes(request)); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 || strings.Join(paths, ",") != "/attachments,/responses" {
		t.Fatalf("attachment routing: proxy=%d paths=%v", count.Load(), paths)
	}
}

func TestHostCredentialFailuresNeverSwitchCredentialsOrGoDirect(t *testing.T) {
	for _, status := range []int{401, 403, 407, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var direct, attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
			defer upstream.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				http.Error(w, "fixture rejected", status)
			}))
			defer proxy.Close()
			svc, _ := credentialTransportService(t, upstream.URL, proxy.URL, "", false)
			_, err := svc.Handle("executor.execute", jsonBytes(websocketRequest(false)))
			var api *APIError
			if !errors.As(err, &api) || api.Status != status || attempts.Load() != 1 || direct.Load() != 0 {
				t.Fatalf("error=%v proxy=%d direct=%d", err, attempts.Load(), direct.Load())
			}
		})
	}
	for _, value := range []string{"broken", "ftp://127.0.0.1:1", "http://fixture:secret@127.0.0.1:1"} {
		t.Run(value, func(t *testing.T) {
			var direct atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
			defer server.Close()
			svc, _ := credentialTransportService(t, server.URL, "", value, false)
			_, err := svc.Handle("executor.execute", jsonBytes(websocketRequest(false)))
			if err == nil || direct.Load() != 0 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid proxy bypass or disclosure: error=%v direct=%d", err, direct.Load())
			}
		})
	}
}

func TestHostCredentialHTTPCancellationBeforeHeaders(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	svc, _ := credentialTransportService(t, server.URL, "", "direct", false)
	svc.cfg.UpstreamTransport = "http"
	result, err := svc.Handle("request.intercept_after", jsonBytes(map[string]any{"RequestID": "fixture-request", "RequestedModel": DefaultModelID}))
	if err != nil {
		t.Fatal(err)
	}
	request := websocketRequest(false)
	request.Headers = objectValue(result)["Headers"].(http.Header)
	done := make(chan error, 1)
	go func() { _, err := svc.Handle("executor.execute", jsonBytes(request)); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not reached")
	}
	_, _ = svc.Handle("request.complete", []byte(`{"RequestID":"fixture-request"}`))
	select {
	case err := <-done:
		var api *APIError
		if !errors.As(err, &api) || api.Status != 499 {
			t.Fatalf("cancellation=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not close credential HTTP connection")
	}
}

func TestHostCredentialHTTPReusesConnections(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonBytes(incrementalTerminal("KEEPALIVE_OK")))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	svc, _ := credentialTransportService(t, server.URL, "", "direct", false)
	for range 3 {
		if _, err := svc.Handle("executor.execute", jsonBytes(websocketRequest(false))); err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("three generations opened %d connections", connections.Load())
	}
}

func TestHostCredentialHTTPBodyLimitAndRedirect(t *testing.T) {
	for _, mode := range []string{"limit", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var destinationHits atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationHits.Add(1) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "redirect" {
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
					return
				}
				_, _ = w.Write([]byte(strings.Repeat("x", 70000)))
			}))
			defer server.Close()
			svc, _ := credentialTransportService(t, server.URL, "", "direct", false)
			svc.cfg.MaxResponseBytes = 65536
			_, err := svc.Handle("executor.execute", jsonBytes(websocketRequest(false)))
			var api *APIError
			if !errors.As(err, &api) {
				t.Fatalf("expected classified failure, got %v", err)
			}
			if mode == "limit" && (api.Status != 502 || api.Kind != "upstream_response_too_large") {
				t.Fatal(err)
			}
			if mode == "redirect" && (api.Status != 307 || destinationHits.Load() != 0) {
				t.Fatalf("redirect replayed or changed status: %v", err)
			}
		})
	}
}

func TestHostCredentialHTTPTimeoutAndShutdown(t *testing.T) {
	for _, mode := range []string{"timeout", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			svc, _ := credentialTransportService(t, server.URL, "", "direct", false)
			svc.cfg.TimeoutSeconds = 1
			done := make(chan error, 1)
			go func() { _, err := svc.Handle("executor.execute", jsonBytes(websocketRequest(false))); done <- err }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream did not start")
			}
			if mode == "shutdown" {
				svc.stopStreams()
			}
			select {
			case err := <-done:
				want := 504
				if mode == "shutdown" {
					want = 503
				}
				var api *APIError
				if !errors.As(err, &api) || api.Status != want {
					t.Fatalf("%s error=%v", mode, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upstream did not stop")
			}
		})
	}
}
