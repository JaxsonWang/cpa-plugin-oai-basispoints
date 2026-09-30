package basispoints

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketInterruptionDiagnostics(t *testing.T) {
	for _, mode := range []string{"non_streaming", "incremental", "buffered"} {
		for _, tc := range []struct {
			name      string
			wire      []byte
			lastEvent string
			events    int
			text      bool
			closeCode int
			logFails  bool
		}{
			{name: "no_event", lastEvent: "none"},
			{name: "created_only", wire: streamFixtureEvents(map[string]any{"type": "response.created", "response": map[string]any{"id": "private-response-id"}}), lastEvent: "response.created", events: 1},
			{name: "text", wire: incrementalPrefix("private-response-text"), lastEvent: "response.output_text.delta", events: 4, text: true},
			{name: "reasoning", wire: reasoningStreamPrefix("private-response-text"), lastEvent: "response.reasoning_summary_text.delta", events: 4, text: true},
			{name: "tool", wire: streamFixtureEvents(map[string]any{"type": "response.function_call_arguments.delta", "delta": "private-tool-arguments"}), lastEvent: "response.function_call_arguments.delta", events: 1},
			{name: "unknown_event", wire: streamFixtureEvents(map[string]any{"type": "private-event\nforged-log=true", "data": "private-event-body"}), lastEvent: "other", events: 1},
			{name: "close_frame_without_terminal", lastEvent: "none", closeCode: websocket.CloseNormalClosure},
			{name: "log_callback_error", lastEvent: "none", logFails: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var creates atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						t.Error(err)
						return
					}
					creates.Add(1)
					if err := writeWebSocketEvents(conn, tc.wire); err != nil {
						t.Error(err)
					}
					if tc.closeCode != 0 {
						if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(tc.closeCode, "private-close-reason"), time.Now().Add(time.Second)); err != nil {
							t.Error(err)
						}
					}
				}))
				defer server.Close()
				svc := newVirtualTestService()
				svc.cfg.ResponsesURL = server.URL
				capture := &websocketCapture{closed: make(chan struct{}, 1)}
				logs := make(chan map[string]any, 2)
				svc.SetHost(func(method string, payload any, out any) error {
					if method == "host.log" {
						logs <- payload.(map[string]any)
						if tc.logFails {
							return errors.New("private-log-callback-failure")
						}
						return nil
					}
					return capture.host(method, payload, out)
				})
				request := websocketRequest(mode != "non_streaming")
				request.HostCallbackID = "diagnostic-fixture-context"
				if mode == "buffered" {
					svc.cfg.StreamToolMode = "buffered"
					request.Payload = jsonBytes(map[string]any{
						"model": DefaultModelID, "input": "hello",
						"tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
					})
				}
				method := "executor.execute"
				if request.Stream {
					method = "executor.execute_stream"
				}
				_, err := svc.Handle(method, jsonBytes(request))
				delivered := mode == "incremental" && tc.text
				var message string
				if delivered {
					if err != nil {
						t.Fatal(err)
					}
					select {
					case <-capture.closed:
					case <-time.After(5 * time.Second):
						t.Fatal("interrupted stream did not close")
					}
					capture.mu.Lock()
					frames := append([][]byte(nil), capture.frames...)
					capture.mu.Unlock()
					failures := 0
					for _, event := range decodeIncrementalFrames(t, request.Format, frames) {
						switch event["type"] {
						case "error":
							failures++
							info := objectValue(event["error"])
							if event["status"] != float64(502) || info["code"] != "upstream_ws_interrupted" {
								t.Fatalf("error classification changed: %v", event)
							}
							message = stringValue(info["message"])
						case "response.completed":
							t.Fatal("interrupted response became successful")
						}
					}
					if failures != 1 {
						t.Fatalf("want one failure, got %d", failures)
					}
				} else {
					var api *APIError
					if !errors.As(err, &api) || api.Status != 502 || api.Kind != "upstream_ws_interrupted" {
						t.Fatalf("error classification changed: %v", err)
					}
					message = api.Message
				}
				svc.streamWG.Wait()
				if len(logs) != 1 {
					t.Fatalf("want one diagnostic log, got %d", len(logs))
				}
				logged := <-logs
				if logged["level"] != "warn" || logged["host_callback_id"] != request.HostCallbackID || logged["message"] != message {
					t.Fatalf("diagnostic log changed context or error: %v", logged)
				}
				if creates.Load() != 1 || capture.fallbacks.Load() != 0 {
					t.Fatalf("request replayed: WS=%d HTTP=%d", creates.Load(), capture.fallbacks.Load())
				}
				code := tc.closeCode
				if code == 0 {
					code = websocket.CloseAbnormalClosure
				}
				var receivedBytes int
				if err := newSSEDecoder().feed(tc.wire, func(_, data string) error {
					receivedBytes += len(data)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{
					"not replayed over HTTP", fmt.Sprintf("close_code=%d", code),
					fmt.Sprintf("events=%d;", tc.events), fmt.Sprintf("received_bytes=%d;", receivedBytes),
					"last_event=" + tc.lastEvent + ";", fmt.Sprintf("delivery_started=%t", delivered),
				} {
					if !strings.Contains(message, want) {
						t.Fatalf("missing %q in %s", want, message)
					}
				}
				assertWebSocketDiagnosticTiming(t, message)
				if strings.Contains(message, "private-") || strings.Contains(message, "forged-log") || strings.ContainsAny(message, "\r\n") {
					t.Fatalf("diagnostic leaked upstream content: %s", message)
				}
			})
		}
	}
}

func assertWebSocketDiagnosticTiming(t *testing.T, message string) {
	t.Helper()
	match := regexp.MustCompile(`elapsed_ms=(\d+); event_idle_ms=(\d+);`).FindStringSubmatch(message)
	if len(match) != 3 {
		t.Fatalf("missing timing: %s", message)
	}
	elapsed, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	idle, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil || idle > elapsed {
		t.Fatalf("invalid event timing: %s", message)
	}
}

func TestWebSocketInterruptionDoesNotExposeTransportError(t *testing.T) {
	now := time.Now()
	progress := websocketProgress{started: now.Add(-4 * time.Second), lastEventAt: now.Add(-time.Second), lastEvent: "none"}
	err := progress.interrupted(fmt.Errorf("private-transport-error with token and address"), nil)
	var api *APIError
	if !errors.As(err, &api) || api.Status != 502 || api.Kind != "upstream_ws_interrupted" {
		t.Fatalf("error classification changed: %v", err)
	}
	assertWebSocketDiagnosticTiming(t, api.Message)
	if strings.Contains(api.Message, "private-") || strings.Contains(api.Message, "close_code=") {
		t.Fatalf("unsafe or fabricated transport diagnostic: %s", api.Message)
	}
}
