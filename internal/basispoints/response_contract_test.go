package basispoints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func relayFixture(id string, broken bool) (map[string]any, string) {
	patch := "*** Begin Patch" + string(rune(10)) + `+await page.locator('[data-id="1"]').click();` + string(rune(10)) + "*** End Patch"
	code, tool := patch, "apply_patch"
	if broken {
		// 自定义输入不再解析为 JSON；函数参数仍严格拒绝漏转义。
		tool = "exec_command"
		code = string(jsonBytes(map[string]any{"cmd": patch}))
		code = strings.ReplaceAll(code, string([]byte{92, 34}), string(rune(34)))
	}
	return map[string]any{
		"type": "function_call", "name": transportName, "call_id": id,
		"arguments": string(jsonBytes(map[string]any{"code": code, "references": []any{tool}})),
	}, patch
}

func TestRelayRegenerationHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"valid", "recover", "exhausted", "legacy_recover", "legacy_exhausted", "upstream_error", "nonstream_sse"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				attempts := 0
				bad, _ := relayFixture(t.Name()+"-bad", true)
				good, patch := relayFixture(t.Name()+"-good", false)
				if strings.HasPrefix(mode, "legacy_") {
					// 旧格式的新输出不猜测修补，由既有有界重生成切换到当前协议。
					code := string(jsonBytes(map[string]any{"tool": "apply_patch", "args": patch}))
					code = strings.ReplaceAll(code, string([]byte{92, 34}), string(rune(34)))
					bad["arguments"] = string(jsonBytes(map[string]any{"code": code, "references": []any{}}))
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts++
					var sent map[string]any
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					if sent["stream"] != stream {
						t.Error("stream mode changed")
					}
					hasHint := strings.Contains(string(jsonBytes(sent)), transportRetryHint)
					if hasHint != (attempts == 2) {
						t.Error("retry hint missing or present on first attempt")
					}
					if mode == "upstream_error" {
						w.WriteHeader(429)
						_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
						return
					}
					native := good
					if strings.HasSuffix(mode, "exhausted") || (strings.HasSuffix(mode, "recover") && attempts == 1) {
						native = bad
					}
					response := map[string]any{"id": "resp_local_fixture", "status": "completed", "output": []any{native}, "usage": map[string]any{"total_tokens": 17}}
					w.Header().Set("X-Request-Id", "local-http-fixture")
					if stream || mode == "nonstream_sse" {
						w.Header().Set("Content-Type", "text/event-stream")
						var events strings.Builder
						writeSSE(&events, "response.completed", map[string]any{"type": "response.completed", "response": response})
						_, _ = io.WriteString(w, events.String())
					} else {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
					}
				}))
				defer server.Close()
				svc := NewService()
				svc.cfg.ResponsesURL = server.URL
				var upstream []byte
				var emitted []byte
				closes := 0
				done := make(chan map[string]any, 1)
				svc.SetHost(func(method string, payload any, out any) error {
					p := payload.(map[string]any)
					switch method {
					case "host.http.do", "host.http.do_stream":
						req, err := http.NewRequest(http.MethodPost, p["url"].(string), bytes.NewReader(p["body"].([]byte)))
						if err != nil {
							return err
						}
						req.Header = p["headers"].(http.Header)
						res, err := server.Client().Do(req)
						if err != nil {
							return err
						}
						defer res.Body.Close()
						raw, err := io.ReadAll(res.Body)
						if err != nil {
							return err
						}
						if method == "host.http.do" {
							*out.(*upstreamResponse) = upstreamResponse{StatusCode: res.StatusCode, Headers: res.Header, Body: raw}
						} else {
							upstream = raw
							*out.(*upstreamStream) = upstreamStream{StatusCode: res.StatusCode, Headers: res.Header, StreamID: "fixture-http"}
						}
					case "host.http.stream_read":
						*out.(*streamChunk) = streamChunk{Payload: upstream, Done: true}
					case "host.http.stream_close":
						closes++
					case "host.stream.emit":
						emitted = p["payload"].([]byte)
					case "host.stream.close":
						done <- p
					default:
						return fmt.Errorf("unexpected method %s", method)
					}
					return nil
				})
				src := map[string]any{"model": DefaultModelID, "input": "Apply patch", "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}, map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}}}}
				req := ExecutorRequest{Model: DefaultModelID, Payload: jsonBytes(src), Stream: stream, StreamID: "client-fixture", StorageJSON: jsonBytes(map[string]any{"access_token": "fixture", "account_id": "fixture"})}
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				result, err := svc.Handle(method, jsonBytes(req))
				wantAttempts := 1
				if strings.HasSuffix(mode, "recover") || strings.HasSuffix(mode, "exhausted") {
					wantAttempts = 2
				}
				if attempts != wantAttempts {
					t.Fatalf("attempts=%d want=%d", attempts, wantAttempts)
				}
				if stream && closes != attempts {
					t.Fatalf("streams leaked: closes=%d attempts=%d", closes, attempts)
				}
				if strings.HasSuffix(mode, "exhausted") || mode == "upstream_error" {
					api, ok := err.(*APIError)
					wantStatus := 422
					if mode == "upstream_error" {
						wantStatus = 429
					}
					if !ok || api.Status != wantStatus {
						t.Fatalf("want status %d, got %v", wantStatus, err)
					}
					if mode == "exhausted" && (!strings.Contains(api.Message, "code invalid_json byte_offset=") || strings.Contains(api.Message, "data-id")) {
						t.Fatalf("unsafe/incomplete diagnostic: %v", api)
					}
					if result != nil || len(emitted) != 0 || len(done) != 0 || rememberedNativeCall(stringValue(bad["call_id"])) != nil {
						t.Fatal("failed response leaked data/state")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var response map[string]any
				if stream {
					select {
					case p := <-done:
						if p["error"] != nil {
							t.Fatal(p)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("stream did not close")
					}
					for _, event := range clientStreamEvents(t, emitted) {
						if event["type"] == "response.completed" {
							response = objectValue(event["response"])
						}
					}
				} else {
					response, _ = rawObject(result.(map[string]any)["Payload"].([]byte))
				}
				call := objectValue(response["output"].([]any)[0])
				if call["type"] != "custom_tool_call" || call["input"] != patch {
					t.Fatal("tool payload changed")
				}
				if rememberedNativeCall(stringValue(bad["call_id"])) != nil {
					t.Fatal("rejected attempt cached")
				}
				t.Logf("HTTP attempts=%d; stream=%t; patch preserved; terminal usage retained", attempts, stream)
			})
		}
	}
}

func TestResponseFormatsAndTerminalStates(t *testing.T) {
	good := map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{messageItem("assistant", "OK")}, "usage": map[string]any{"total_tokens": 23}}
	sse := func(kind string, r map[string]any) []byte {
		var b strings.Builder
		writeSSE(&b, kind, map[string]any{"type": kind, "response": r})
		return []byte(b.String())
	}
	incomplete := cloneObject(good)
	incomplete["status"] = "incomplete"
	incomplete["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	for _, tc := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"json", jsonBytes(good), "completed"},
		{"sse", sse("response.completed", good), "completed"},
		{"sse_eof", bytes.TrimSpace(sse("response.completed", good)), "completed"},
		{"incomplete_json", jsonBytes(incomplete), "incomplete"},
		{"incomplete_sse", sse("response.incomplete", incomplete), "incomplete"},
		{"html", []byte("<html>PRIVATE_BODY</html>"), ""},
		{"empty", nil, ""},
		{"null", []byte("null"), ""},
		{"missing_status", jsonBytes(map[string]any{"output": []any{}}), ""},
		{"unknown_status", jsonBytes(map[string]any{"status": "unknown", "output": []any{}}), ""},
		{"trailing", append(jsonBytes(good), []byte("junk")...), ""},
		{"failure", sse("response.failed", good), ""},
		{"status_mismatch", sse("response.completed", incomplete), ""},
		{"no_terminal", []byte("data: [DONE]"), ""},
		{"invalid_event", []byte("data: {PRIVATE_BODY"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := parseResponse(tc.raw, http.Header{"Content-Type": {"text/event-stream"}})
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid response accepted")
				}
				if strings.Contains(err.Error(), "PRIVATE_BODY") {
					t.Fatal("response data leaked")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if response["status"] != tc.want || fmt.Sprint(objectValue(response["usage"])["total_tokens"]) != "23" {
				t.Fatalf("terminal status/usage lost: %v", response)
			}
			if tc.want == "incomplete" {
				wire := string(syntheticStream(response))
				if strings.Contains(wire, "response.completed") || !strings.Contains(wire, "response.incomplete") {
					t.Fatal("incomplete synthesized as success")
				}
			}
		})
	}
}
