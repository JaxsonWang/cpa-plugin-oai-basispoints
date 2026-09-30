package basispoints

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestBufferedToolStreamRegeneration(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, lead := range []string{"text", "reasoning"} {
			for _, mode := range []string{"recover", "exhausted"} {
				t.Run(format+"/"+lead+"/"+mode, func(t *testing.T) {
					svc := newHTTPTestService()
					if err := svc.configure(jsonBytes(map[string]any{"config_yaml": []byte("data_dir: ''\ncredential_source: virtual\nupstream_transport: http\nstream_tool_mode: buffered\n")})); err != nil {
						t.Fatal(err)
					}
					bad, _ := relayFixture(t.Name()+"-bad", true)
					good, patch := relayFixture(t.Name()+"-good", false)
					attempts, reads, upstreamCloses, clientCloses := 0, 0, 0, 0
					var frames [][]byte
					early := false
					svc.SetHost(func(method string, payload any, out any) error {
						switch method {
						case "host.http.do_stream":
							attempts++
							reads = 0
							body := payload.(map[string]any)["body"].([]byte)
							if bytes.Contains(body, []byte(transportRetryHint)) != (attempts == 2) {
								return fmt.Errorf("unexpected regeneration hint on attempt %d", attempts)
							}
							*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: fmt.Sprint(attempts)}
						case "host.http.stream_read":
							reads++
							text := fmt.Sprintf("第 %d 次正文：中文、引号和换行\n", attempts)
							prefix := incrementalPrefix(text)
							response := incrementalTerminal(text, good)
							if lead == "reasoning" {
								prefix = reasoningStreamPrefix(text)
								response["output"] = []any{reasoningStreamItem(text), good}
							}
							if attempts == 1 || mode == "exhausted" {
								response["output"] = append(response["output"].([]any), bad)
							}
							if reads == 1 {
								*out.(*streamChunk) = streamChunk{Payload: prefix}
							} else {
								*out.(*streamChunk) = streamChunk{Payload: streamFixtureEvents(map[string]any{"type": "response.completed", "response": response}), Done: true}
							}
						case "host.http.stream_close":
							upstreamCloses++
						case "host.stream.emit":
							early = early || attempts != 2 || reads != 2
							frames = append(frames, bytes.Clone(payload.(map[string]any)["payload"].([]byte)))
						case "host.stream.close":
							clientCloses++
						default:
							return fmt.Errorf("unexpected callback %s", method)
						}
						return nil
					})
					source := map[string]any{"input": "hello", "tools": []any{
						map[string]any{"type": "custom", "name": "apply_patch"},
						map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
					}}
					result, err := svc.Handle("executor.execute_stream", jsonBytes(ExecutorRequest{Model: DefaultModelID, Format: format, SourceFormat: format, Stream: true, StreamID: "client", Payload: jsonBytes(source), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}))
					svc.streamWG.Wait()
					if attempts != 2 || upstreamCloses != 2 || early {
						t.Fatalf("attempts=%d upstream_closes=%d early_delivery=%t; want two attempts before any delivery", attempts, upstreamCloses, early)
					}
					if rememberedNativeCall(stringValue(bad["call_id"])) != nil {
						t.Fatal("rejected call entered history cache")
					}
					if mode == "exhausted" {
						api, ok := err.(*APIError)
						if !ok || api.Status != 422 || api.Kind != "invalid_tool_call" || !strings.Contains(api.Message, "code invalid_json byte_offset=") || strings.Contains(api.Message, "data-id") {
							t.Fatalf("expected safe invalid-tool error, got %v", err)
						}
						if result != nil || len(frames) != 0 || clientCloses != 0 || rememberedNativeCall(stringValue(good["call_id"])) != nil {
							t.Fatal("failed attempts leaked response or tool state")
						}
						return
					}
					if err != nil || clientCloses != 1 {
						t.Fatalf("err=%v client_closes=%d", err, clientCloses)
					}
					var text string
					tools, terminals := 0, 0
					for _, event := range decodeIncrementalFrames(t, format, frames) {
						switch event["type"] {
						case "response.output_text.delta", "response.reasoning_summary_text.delta":
							text += event["delta"].(string)
						case "response.output_item.done":
							item := objectValue(event["item"])
							if item["type"] == "custom_tool_call" {
								tools++
								if item["input"] != patch || item["call_id"] != good["call_id"] {
									t.Fatal("tool payload or identity changed")
								}
							}
						case "response.completed":
							terminals++
							if objectValue(objectValue(event["response"])["usage"])["total_tokens"] != float64(17) {
								t.Fatal("final usage lost")
							}
						case "error":
							t.Fatal("rejected attempt leaked an error event")
						}
					}
					if text != "第 2 次正文：中文、引号和换行\n" || tools != 1 || terminals != 1 {
						t.Fatalf("text=%q tools=%d terminals=%d", text, tools, terminals)
					}
				})
			}
		}
	}
}

func TestStreamToolModeConfiguration(t *testing.T) {
	svc := NewService()
	if svc.config().StreamToolMode != "incremental" {
		t.Fatal("default incremental delivery changed")
	}
	for _, mode := range []string{"buffered", "incremental", " BUFFERED "} {
		if err := svc.configure(jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: ''\nstream_tool_mode: %q\n", mode))})); err != nil {
			t.Fatal(err)
		}
		want := strings.ToLower(strings.TrimSpace(mode))
		if svc.config().StreamToolMode != want || svc.status()["stream_tool_mode"] != want {
			t.Fatal("configured mode not exposed")
		}
	}
	for _, mode := range []string{"", "auto", "unsafe"} {
		err := svc.configure(jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: ''\nstream_tool_mode: %q\n", mode))}))
		if api, ok := err.(*APIError); !ok || api.Status != 400 || api.Kind != "invalid_config" || svc.config().StreamToolMode != "buffered" {
			t.Fatalf("invalid configuration accepted or active setting changed: %v", err)
		}
	}
	dir := filepath.Join(t.TempDir(), "settings")
	if err := svc.configure(jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: %q\nstream_tool_mode: buffered\n", dir))})); err != nil {
		t.Fatal(err)
	}
	restored := NewService()
	if err := restored.configure(jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: %q\n", dir))})); err != nil {
		t.Fatal(err)
	}
	if restored.config().StreamToolMode != "buffered" {
		t.Fatal("persisted mode lost")
	}
	found := false
	for _, field := range registration(svc.config())["metadata"].(map[string]any)["ConfigFields"].([]map[string]any) {
		found = found || field["Name"] == "stream_tool_mode"
	}
	if !found {
		t.Fatal("configuration field not registered")
	}
}

func TestBufferedToolStreamWebSocket(t *testing.T) {
	for _, mode := range []string{"recover", "exhausted", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			var attempts atomic.Int32
			bad, _ := relayFixture(t.Name()+"-bad", true)
			good := namespaceTestNative(t.Name()+"-good", "exec_command", map[string]any{"cmd": "printf '%s\\n' \"中文\""})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				var sent map[string]any
				if err := conn.ReadJSON(&sent); err != nil {
					t.Error(err)
					return
				}
				attempt := attempts.Add(1)
				if strings.Contains(string(jsonBytes(sent)), transportRetryHint) != (attempt == 2) {
					t.Error("unexpected regeneration hint")
				}
				text := fmt.Sprintf("attempt-%d", attempt)
				if err := writeWebSocketEvents(conn, incrementalPrefix(text)); err != nil {
					t.Error(err)
					return
				}
				if mode == "interrupted" {
					return
				}
				call := bad
				if attempt == 2 && mode == "recover" {
					call = good
				}
				if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": incrementalTerminal(text, call)}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			svc := newVirtualTestService()
			svc.cfg.StreamToolMode, svc.cfg.ResponsesURL = "buffered", server.URL
			capture := &websocketCapture{closed: make(chan struct{}, 1)}
			svc.SetHost(capture.host)
			request := websocketRequest(true)
			request.Payload = jsonBytes(map[string]any{"input": "hello", "tools": []any{map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}}}})
			_, err := svc.Handle("executor.execute_stream", jsonBytes(request))
			svc.streamWG.Wait()
			wantAttempts := int32(2)
			if mode == "interrupted" {
				wantAttempts = 1
			}
			if attempts.Load() != wantAttempts || capture.fallbacks.Load() != 0 || rememberedNativeCall(stringValue(bad["call_id"])) != nil {
				t.Fatalf("attempts=%d HTTP fallback=%d", attempts.Load(), capture.fallbacks.Load())
			}
			if mode != "recover" {
				status, kind := 422, "invalid_tool_call"
				if mode == "interrupted" {
					status, kind = 502, "upstream_ws_interrupted"
				}
				if api, ok := err.(*APIError); !ok || api.Status != status || api.Kind != kind || len(capture.frames) != 0 {
					t.Fatalf("failure leaked content or classification: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var text string
			tools, terminals := 0, 0
			for _, event := range decodeIncrementalFrames(t, request.Format, capture.frames) {
				if event["type"] == "response.output_text.delta" {
					text += event["delta"].(string)
				}
				if event["type"] == "response.output_item.done" && objectValue(event["item"])["type"] == "function_call" {
					tools++
					if objectValue(event["item"])["arguments"] != `{"cmd":"printf '%s\\n' \"中文\""}` {
						t.Fatal("function arguments changed")
					}
				}
				if event["type"] == "response.completed" {
					terminals++
				}
			}
			if text != "attempt-2" || tools != 1 || terminals != 1 {
				t.Fatalf("text=%q tools=%d terminals=%d", text, tools, terminals)
			}
		})
	}
}

func TestBufferedToolStreamCancellation(t *testing.T) {
	for _, mode := range []string{"quiesce", "request_complete", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			svc := newVirtualTestService()
			svc.cfg.StreamToolMode = "buffered"
			if mode == "timeout" {
				svc.cfg.TimeoutSeconds = 1
			}
			if _, err := svc.Handle("request.intercept_after", jsonBytes(map[string]any{"RequestID": "cancel-fixture", "Model": DefaultModelID})); err != nil {
				t.Fatal(err)
			}
			waiting, upstreamClosed := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var reads, attempts, emits atomic.Int32
			svc.SetHost(func(method string, payload any, out any) error {
				switch method {
				case "host.http.do_stream":
					attempts.Add(1)
					*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "cancelable"}
				case "host.http.stream_read":
					if reads.Add(1) == 1 {
						*out.(*streamChunk) = streamChunk{Payload: incrementalPrefix("not delivered")}
					} else {
						close(waiting)
						select {
						case <-upstreamClosed:
						case <-time.After(5 * time.Second):
							return fmt.Errorf("buffered stream not canceled")
						}
						*out.(*streamChunk) = streamChunk{Done: true}
					}
				case "host.http.stream_close":
					once.Do(func() { close(upstreamClosed) })
				case "host.stream.emit", "host.stream.close":
					emits.Add(1)
				default:
					return fmt.Errorf("unexpected callback %s", method)
				}
				return nil
			})
			done := make(chan error, 1)
			go func() {
				_, err := svc.Handle("executor.execute_stream", jsonBytes(ExecutorRequest{Model: DefaultModelID, Format: "openai-response", Stream: true, StreamID: "client", Headers: http.Header{requestLifecycleHeader: {"cancel-fixture"}}, Payload: jsonBytes(map[string]any{"input": "hello", "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}}}), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}))
				done <- err
			}()
			select {
			case <-waiting:
			case <-time.After(5 * time.Second):
				t.Fatal("stream did not reach buffered state")
			}
			wantStatus := 504
			if mode == "quiesce" {
				wantStatus = 503
				if _, err := svc.Handle("plugin.quiesce", nil); err != nil {
					t.Fatal(err)
				}
			} else if mode == "request_complete" {
				wantStatus = 499
				if _, err := svc.Handle("request.complete", jsonBytes(map[string]any{"RequestID": "cancel-fixture"})); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if api, ok := err.(*APIError); !ok || api.Status != wantStatus {
					t.Fatalf("expected status %d, got %v", wantStatus, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled request did not return")
			}
			svc.streamWG.Wait()
			if emits.Load() != 0 || attempts.Load() != 1 {
				t.Fatal("canceled response emitted or regenerated")
			}
		})
	}
}

func TestBufferedToolStreamScopeAndFailures(t *testing.T) {
	for _, scenario := range []string{"no_tools", "none", "text_only", "terminal_only", "namespace", "dynamic_original", "required_missing", "incomplete", "incomplete_tool", "request_failure", "rate_limit", "transport", "truncated", "size_limit", "changed_text", "changed_id", "invalid_lifecycle"} {
		t.Run(scenario, func(t *testing.T) {
			svc := newHTTPTestService()
			svc.cfg.StreamToolMode = "buffered"
			good, _ := relayFixture(t.Name()+"-good", false)
			bad, _ := relayFixture(t.Name()+"-bad", true)
			source := map[string]any{"input": "hello", "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}}}
			text := "  保留空白\r\n"
			prefix := incrementalPrefix(text)
			response := incrementalTerminal(text, good)
			wantAttempts, wantStatus := 1, 0
			wantKind := ""
			switch scenario {
			case "text_only":
				response = incrementalTerminal(text)
			case "terminal_only":
				prefix = nil
			case "no_tools", "none":
				response = incrementalTerminal(text)
				if scenario == "no_tools" {
					delete(source, "tools")
				} else {
					source["tool_choice"] = "none"
				}
			case "namespace", "dynamic_original":
				source = namespaceTestSource("function", "js", "mcp__node_repl")
				good = namespaceTestNative(t.Name()+"-good", "mcp__node_repl.js", map[string]any{"code": "console.log(\"中文\")\n"})
				response = incrementalTerminal(text, good)
				if scenario == "dynamic_original" {
					source["input"] = []any{map[string]any{"type": "additional_tools", "tools": source["tools"]}, messageItem("user", "hello")}
					delete(source, "tools")
				}
			case "required_missing":
				source["tool_choice"] = "required"
				response = incrementalTerminal(text)
				wantAttempts, wantStatus, wantKind = 2, 422, "invalid_tool_call"
			case "incomplete", "incomplete_tool":
				response["status"] = "incomplete"
				response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				if scenario == "incomplete_tool" {
					response["output"] = append(response["output"].([]any), bad)
					wantStatus, wantKind = 422, "invalid_tool_call"
				}
			case "changed_text":
				response = incrementalTerminal("different", good)
				wantStatus, wantKind = 502, "invalid_upstream_stream"
			case "changed_id":
				response["id"] = "different"
				wantStatus, wantKind = 502, "invalid_upstream_stream"
			case "invalid_lifecycle":
				prefix = append(prefix, streamFixtureEvents(map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": "msg_incremental", "text": "different"})...)
				wantStatus, wantKind = 502, "invalid_upstream_stream"
			}
			last := streamChunk{Payload: streamFixtureEvents(map[string]any{"type": "response." + stringValue(response["status"]), "response": response}), Done: true}
			switch scenario {
			case "request_failure":
				last.Payload = streamFixtureEvents(map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"code": "context_length_exceeded", "message": "private-fixture"}}})
				wantStatus, wantKind = 400, "context_length_exceeded"
			case "rate_limit":
				last.Payload = streamFixtureEvents(map[string]any{"type": "error", "status": 429, "error": map[string]any{"code": "rate_limit_exceeded", "message": "private-fixture"}})
				wantStatus, wantKind = 429, "rate_limit_exceeded"
			case "transport":
				last.Payload, last.Error = nil, "connection reset"
				wantStatus, wantKind = 502, "upstream_transport"
			case "truncated":
				last.Payload = nil
				wantStatus = 502
			case "size_limit":
				svc.cfg.MaxResponseBytes = 3000
				last.Payload = bytes.Repeat([]byte("x"), 4000)
				wantStatus, wantKind = 502, "upstream_response_too_large"
			}
			attempts, reads, closes, clientCloses := 0, 0, 0, 0
			var frames [][]byte
			early := false
			svc.SetHost(func(method string, payload any, out any) error {
				switch method {
				case "host.http.do_stream":
					attempts++
					reads = 0
					*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "fixture"}
				case "host.http.stream_read":
					reads++
					if reads == 1 {
						*out.(*streamChunk) = streamChunk{Payload: prefix}
					} else {
						*out.(*streamChunk) = last
					}
				case "host.http.stream_close":
					closes++
				case "host.stream.emit":
					early = early || reads == 1
					frames = append(frames, bytes.Clone(payload.(map[string]any)["payload"].([]byte)))
				case "host.stream.close":
					clientCloses++
				default:
					return fmt.Errorf("unexpected callback %s", method)
				}
				return nil
			})
			request := ExecutorRequest{Model: DefaultModelID, Format: "openai-response", Stream: true, StreamID: "client", Payload: jsonBytes(source), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}
			if scenario == "dynamic_original" {
				request.OriginalRequest, request.Payload = request.Payload, []byte(`{"input":"translated request without tools"}`)
			}
			result, err := svc.Handle("executor.execute_stream", jsonBytes(request))
			svc.streamWG.Wait()
			if attempts != wantAttempts || closes != attempts || early != (scenario == "no_tools" || scenario == "none") {
				t.Fatalf("attempts=%d closes=%d early=%t", attempts, closes, early)
			}
			if wantStatus != 0 {
				api, ok := err.(*APIError)
				if !ok || api.Status != wantStatus || (wantKind != "" && api.Kind != wantKind) || strings.Contains(api.Message, "private-fixture") {
					t.Fatalf("expected %d/%s, got %v", wantStatus, wantKind, err)
				}
				if result != nil || len(frames) != 0 || clientCloses != 0 || rememberedNativeCall(stringValue(good["call_id"])) != nil {
					t.Fatal("failed response delivered or cached")
				}
				return
			}
			if err != nil || clientCloses != 1 {
				t.Fatalf("err=%v client_closes=%d", err, clientCloses)
			}
			var actual string
			terminals := 0
			for _, event := range decodeIncrementalFrames(t, request.Format, frames) {
				if event["type"] == "response.output_text.delta" {
					actual += event["delta"].(string)
				}
				if event["type"] == "response."+stringValue(response["status"]) {
					terminals++
				}
				if event["type"] == "error" {
					t.Fatal("unexpected stream failure")
				}
			}
			if actual != text || terminals != 1 {
				t.Fatalf("text=%q terminals=%d", actual, terminals)
			}
		})
	}
}
