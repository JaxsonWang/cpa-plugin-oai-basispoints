package basispoints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketFirstAndHandshakeFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{101, 404, 503, 401, 403, 429} {
			t.Run(fmt.Sprintf("stream=%t/status=%d", stream, status), func(t *testing.T) {
				var upgrades, creates, fallbacks atomic.Int32
				response := incrementalTerminal("WS_OK")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upgrades.Add(1)
					if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
						t.Error("WS 握手丢失了方法或鉴权信息")
					}
					if status != 101 {
						http.Error(w, `{"error":{"message":"fixture handshake rejected"}}`, status)
						return
					}
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					var request map[string]any
					if err := conn.ReadJSON(&request); err != nil {
						t.Error(err)
						return
					}
					creates.Add(1)
					if request["type"] != "response.create" || request["stream"] != nil || request["model"] != "gpt-6-astra" {
						t.Errorf("WS 请求格式不正确: %s", jsonBytes(request))
					}
					_ = newSSEDecoder().feed(syntheticStream(response), func(_ string, data string) error {
						if data == "[DONE]" {
							return nil
						}
						return conn.WriteMessage(websocket.TextMessage, []byte(data))
					})
				}))
				defer server.Close()
				svc := NewService()
				svc.cfg.ResponsesURL = server.URL + "/responses"
				closed := make(chan struct{}, 1)
				var frames [][]byte
				svc.SetHost(func(method string, payload any, out any) error {
					switch method {
					case "host.http.do":
						fallbacks.Add(1)
						*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(response)}
					case "host.http.do_stream":
						fallbacks.Add(1)
						*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "sse"}
					case "host.http.stream_read":
						*out.(*streamChunk) = streamChunk{Payload: syntheticStream(response), Done: true}
					case "host.http.stream_close", "host.log":
					case "host.stream.emit":
						frames = append(frames, bytes.Clone(payload.(map[string]any)["payload"].([]byte)))
					case "host.stream.close":
						closed <- struct{}{}
					default:
						return fmt.Errorf("unexpected callback %s", method)
					}
					return nil
				})
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				result, err := svc.Handle(method, jsonBytes(ExecutorRequest{Model: DefaultModelID, Format: "openai-response", Stream: stream, StreamID: "client", Payload: jsonBytes(map[string]any{"model": DefaultModelID, "input": "Reply WS_OK."}), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture-account", "websockets": true}}))
				hardFailure := status == 401 || status == 403 || status == 429
				if hardFailure {
					api, ok := err.(*APIError)
					if !ok || api.Status != status {
						t.Fatalf("鉴权/限流错误应原样返回: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if stream {
						select {
						case <-closed:
						case <-time.After(5 * time.Second):
							t.Fatal("stream did not close")
						}
						if !bytes.Contains(bytes.Join(frames, nil), []byte("WS_OK")) {
							t.Fatal("missing text")
						}
					} else {
						var actual map[string]any
						if json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &actual) != nil || actual["status"] != "completed" {
							t.Fatal("invalid non-stream response")
						}
					}
				}
				svc.streamWG.Wait()
				wantFallback := int32(0)
				if status != 101 && !hardFailure {
					wantFallback = 1
				}
				wantCreates := int32(0)
				if status == 101 {
					wantCreates = 1
				}
				if upgrades.Load() != 1 || creates.Load() != wantCreates || fallbacks.Load() != wantFallback {
					t.Fatalf("upgrades=%d creates=%d fallbacks=%d; want 1/%d/%d", upgrades.Load(), creates.Load(), fallbacks.Load(), wantCreates, wantFallback)
				}
			})
		}
	}
}
