package basispoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAgentMessageRoutingTranslation(t *testing.T) {
	content := []any{
		map[string]any{"type": "input_text", "text": "第一段\n  keep whitespace\n", "author": "nested-author"},
		map[string]any{"type": "input_text", "text": "第二段", "recipient": "nested-recipient"},
	}
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{
		{"both", map[string]any{"author": "/root", "recipient": "/root/child"}},
		{"author-only", map[string]any{"author": "/root/child"}},
		{"recipient-only", map[string]any{"recipient": "/root"}},
		{"escaped", map[string]any{"author": "/root/\"quoted\"\nchild", "recipient": "/root"}},
		{"explicit-empty", map[string]any{"author": "", "recipient": nil}},
		{"no-routing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]any{"type": "agent_message", "id": "amsg_fixture", "content": content}
			for key, value := range tc.fields {
				item[key] = value
			}
			before := string(jsonBytes(item))
			got := translateInputItems([]any{item})
			want := []any{item}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("translated=%s want=%s", jsonBytes(got), jsonBytes(want))
			}
			if string(jsonBytes(item)) != before {
				t.Fatal("mutated client history")
			}
			if again := translateInputItems(got); !reflect.DeepEqual(again, got) {
				t.Fatal("repeated translation changed native routing")
			}
		})
	}
	for _, kind := range []string{"message", "unknown_future_item"} {
		t.Run(kind, func(t *testing.T) {
			item := map[string]any{"type": kind, "author": "keep-author", "recipient": "keep-recipient", "content": content}
			if got := translateInputItems([]any{item}); !reflect.DeepEqual(got, []any{item}) {
				t.Fatal("modified routing fields outside agent_message")
			}
		})
	}
}

func TestAgentMessageRoutingPreservesInvalidContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content map[string]any
	}{
		{"missing", nil},
		{"null", map[string]any{"content": nil}},
		{"empty", map[string]any{"content": []any{}}},
		{"string", map[string]any{"content": "not an array"}},
		{"object", map[string]any{"content": map[string]any{"type": "input_text", "text": "not an array"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]any{"type": "agent_message", "author": "/root", "recipient": "/root/child"}
			for key, value := range tc.content {
				item[key] = value
			}
			before := string(jsonBytes(item))
			if got := translateInputItems([]any{item}); !reflect.DeepEqual(got, []any{item}) {
				t.Fatalf("manufactured or changed invalid content: %s", jsonBytes(got))
			}
			if string(jsonBytes(item)) != before {
				t.Fatal("mutated client history")
			}
		})
	}
}

func TestAgentMessageRoutingHTTPWireEncoding(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				svc := newHTTPTestService()
				calls := 0
				svc.SetHost(func(method string, payload any, out any) error {
					calls++
					wantMethod := "host.http.do"
					if stream {
						wantMethod = "host.http.do_stream"
					}
					if method != wantMethod {
						t.Fatalf("host method=%s want=%s", method, wantMethod)
					}
					var wire map[string]any
					if err := json.Unmarshal(payload.(map[string]any)["body"].([]byte), &wire); err != nil {
						t.Fatal(err)
					}
					items := wire["input"].([]any)
					want := map[string]any{"type": "agent_message", "author": "/root/child", "recipient": "/root", "content": []any{
						map[string]any{"type": "input_text", "text": "Read-only result\n中文  text"},
					}}
					if got := items[len(items)-1]; !reflect.DeepEqual(got, want) {
						t.Fatalf("wire agent_message=%s want=%s", jsonBytes(got), jsonBytes(want))
					}
					if stream {
						*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "routing-fixture"}
					} else {
						*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200}
					}
					return nil
				})
				source := map[string]any{"model": DefaultModelID, "input": []any{map[string]any{
					"type": "agent_message", "author": "/root/child", "recipient": "/root",
					"internal_chat_message_metadata_passthrough": map[string]any{"client": "fixture"},
					"content": []any{map[string]any{"type": "input_text", "text": "Read-only result\n中文  text"}},
				}}}
				req := ExecutorRequest{Model: DefaultModelID, Format: format, SourceFormat: format, Stream: stream, Payload: jsonBytes(source), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}
				if format == "codex" {
					req.OriginalRequest = jsonBytes(map[string]any{"model": "ignored-original"})
				}
				body, cred, err := svc.prepareRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				if stream {
					_, err = svc.upstreamStream(req, body, cred)
				} else {
					_, err = svc.upstreamRequest(req, body, cred, false)
				}
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("host calls=%d want=1", calls)
				}
			})
		}
	}
}

func TestAgentMessageRoutingWebSocketWireEncoding(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				captured := make(chan map[string]any, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
						return r.Header.Get("Origin") == "https://bps.openai.com"
					}}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					var wire map[string]any
					if err := conn.ReadJSON(&wire); err != nil {
						t.Error(err)
						return
					}
					captured <- wire
					if err := newSSEDecoder().feed(syntheticStream(incrementalTerminal("ROUTING_OK")), func(_ string, data string) error {
						if data == "[DONE]" {
							return nil
						}
						return conn.WriteMessage(websocket.TextMessage, []byte(data))
					}); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				svc := NewService()
				svc.cfg.ResponsesURL = server.URL + "/responses"
				closed := make(chan struct{}, 1)
				svc.SetHost(func(method string, payload any, out any) error {
					switch method {
					case "host.stream.emit":
					case "host.stream.close":
						closed <- struct{}{}
					default:
						return fmt.Errorf("unexpected host callback: %s", method)
					}
					return nil
				})
				source := map[string]any{"model": DefaultModelID, "input": []any{map[string]any{
					"type": "agent_message", "author": "/root/child", "recipient": "/root",
					"content": []any{map[string]any{"type": "input_text", "text": "Read-only result\n中文  text"}},
				}}}
				req := ExecutorRequest{Model: DefaultModelID, Format: format, SourceFormat: format, Stream: stream, StreamID: "routing-client", Payload: jsonBytes(source), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture", "websockets": true}}
				if format == "codex" {
					req.OriginalRequest = jsonBytes(map[string]any{"model": "ignored-original"})
				}
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				if _, err := svc.Handle(method, jsonBytes(req)); err != nil {
					t.Fatal(err)
				}
				if stream {
					select {
					case <-closed:
					case <-time.After(5 * time.Second):
						t.Fatal("stream did not close")
					}
				}
				svc.streamWG.Wait()
				var wire map[string]any
				select {
				case wire = <-captured:
				default:
					t.Fatal("missing WebSocket request")
				}
				if wire["type"] != "response.create" {
					t.Fatalf("unexpected WebSocket frame: %s", jsonBytes(wire))
				}
				items := wire["input"].([]any)
				want := map[string]any{"type": "agent_message", "author": "/root/child", "recipient": "/root", "content": []any{
					map[string]any{"type": "input_text", "text": "Read-only result\n中文  text"},
				}}
				if got := items[len(items)-1]; !reflect.DeepEqual(got, want) {
					t.Fatalf("WebSocket agent_message=%s want=%s", jsonBytes(got), jsonBytes(want))
				}
			})
		}
	}
}

// 根据实际 400 的必填字段约束校验请求；这是本地协议夹具，不是真实上游验收。
func TestAgentMessageRequiredAuthor(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, stream := range []bool{false, true} {
			for _, includeAuthor := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/author=%t", format, stream, includeAuthor), func(t *testing.T) {
					svc := newHTTPTestService()
					var responseBody []byte
					calls := 0
					svc.SetHost(func(method string, payload any, out any) error {
						switch method {
						case "host.http.do", "host.http.do_stream":
							calls++
							var wire map[string]any
							if err := json.Unmarshal(payload.(map[string]any)["body"].([]byte), &wire); err != nil {
								return err
							}
							status := http.StatusOK
							response := incrementalTerminal("AUTHOR_OK")
							responseBody = jsonBytes(response)
							if stream {
								responseBody = syntheticStream(response)
							}
							for index, raw := range wire["input"].([]any) {
								item := objectValue(raw)
								if item["type"] != "agent_message" {
									continue
								}
								if _, present := item["author"]; !present {
									status = http.StatusBadRequest
									param := fmt.Sprintf("input[%d].author", index)
									responseBody = jsonBytes(map[string]any{"error": map[string]any{
										"type": "invalid_request_error", "code": "missing_required_parameter", "param": param,
										"message": fmt.Sprintf("[ObjectParam] [%s] [missing_required_parameter] Missing required parameter: '%s'.", param, param),
									}})
								}
							}
							if method == "host.http.do" {
								*out.(*upstreamResponse) = upstreamResponse{StatusCode: status, Body: responseBody}
							} else {
								*out.(*upstreamStream) = upstreamStream{StatusCode: status, StreamID: "required-author"}
							}
						case "host.http.stream_read":
							*out.(*streamChunk) = streamChunk{Payload: responseBody, Done: true}
						case "host.http.stream_close", "host.stream.emit", "host.stream.close":
						default:
							return fmt.Errorf("unexpected host callback: %s", method)
						}
						return nil
					})
					history := make([]any, 0, 29)
					for range 28 {
						history = append(history, messageItem("user", "Existing history"))
					}
					message := map[string]any{"type": "agent_message", "recipient": "/root", "content": []any{map[string]any{"type": "input_text", "text": "Original result"}}}
					if includeAuthor {
						message["author"] = "/root/child"
					}
					history = append(history, message)
					source := map[string]any{"model": DefaultModelID, "reasoning": map[string]any{"effort": "xhigh"}, "input": history}
					req := ExecutorRequest{Model: DefaultModelID, Format: format, SourceFormat: format, Stream: stream, StreamID: "client-required-author", Payload: jsonBytes(source), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}
					if format == "codex" {
						req.OriginalRequest = jsonBytes(map[string]any{"model": "ignored-original"})
					}
					method := "executor.execute"
					if stream {
						method = "executor.execute_stream"
					}
					_, err := svc.Handle(method, jsonBytes(req))
					svc.streamWG.Wait()
					if calls != 1 {
						t.Fatalf("upstream calls=%d want=1", calls)
					}
					if includeAuthor {
						if err != nil {
							t.Fatalf("valid native agent_message was rejected: %v", err)
						}
					} else {
						var apiError *APIError
						if !errors.As(err, &apiError) || apiError.Status != 400 || !strings.Contains(err.Error(), "input[29].author") {
							t.Fatalf("missing author was fabricated or its upstream rejection was hidden: %v", err)
						}
					}
				})
			}
		}
	}
}
