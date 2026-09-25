package basispoints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func translatedCodexRequest(stream bool) ExecutorRequest {
	source := namespaceTestSource("function", "echo", "")
	source["model"] = DefaultModelID
	source["input"] = []any{messageItem("developer", "Translated system."), messageItem("user", "Translated prompt.")}
	return ExecutorRequest{Model: DefaultModelID, Format: "codex", SourceFormat: "codex", Stream: stream, StreamID: "translated-client", Payload: jsonBytes(source), OriginalRequest: jsonBytes(map[string]any{"model": DefaultModelID, "messages": []any{map[string]any{"role": "user", "content": "Original Claude prompt."}}, "tools": []any{map[string]any{"name": "echo", "input_schema": map[string]any{"type": "object"}}}}), StorageJSON: jsonBytes(map[string]any{"access_token": "fixture", "account_id": "fixture"})}
}

func TestTranslatedCodexInputUsesHostPayload(t *testing.T) {
	req := translatedCodexRequest(false)
	body, _, err := NewService().prepareRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	text := string(jsonBytes(body["input"]))
	if !strings.Contains(text, "Translated prompt.") || !strings.Contains(text, "Translated system.") || !strings.Contains(text, "- echo (function)") || strings.Contains(text, "Original Claude prompt.") {
		t.Fatalf("wrong protocol source: %s", text)
	}
	req.Payload = nil
	if _, _, err := NewService().prepareRequest(req); err == nil {
		t.Fatal("missing translated payload was silently replaced by raw Claude input")
	}
}

func TestCodexHostOutputContract(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []string{"completed", "incomplete"} {
			for _, tool := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/status=%s/tool=%t", stream, status, tool), func(t *testing.T) {
					svc := NewService()
					req := translatedCodexRequest(stream)
					output := messageItem("assistant", "TRANSLATED_OK")
					if tool {
						output = namespaceTestNative(t.Name(), "echo", map[string]any{"value": "fixture-value"})
					}
					response := map[string]any{"id": "resp_translated", "status": status, "model": DefaultUpstreamModel, "output": []any{output}, "usage": map[string]any{"input_tokens": 11, "output_tokens": 6, "total_tokens": 17}}
					if status == "incomplete" {
						response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					}
					terminal := "response." + status
					var events strings.Builder
					writeSSE(&events, terminal, map[string]any{"type": terminal, "response": response})
					var chunks [][]byte
					closed := make(chan map[string]any, 1)
					svc.SetHost(func(method string, payload any, out any) error {
						switch method {
						case "host.http.do":
							*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: jsonBytes(response)}
						case "host.http.do_stream":
							*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "translated-upstream"}
						case "host.http.stream_read":
							*out.(*streamChunk) = streamChunk{Payload: []byte(events.String()), Done: true}
						case "host.http.stream_close":
						case "host.stream.emit":
							chunks = append(chunks, bytes.Clone(payload.(map[string]any)["payload"].([]byte)))
						case "host.stream.close":
							closed <- payload.(map[string]any)
						default:
							return fmt.Errorf("unexpected callback %s", method)
						}
						return nil
					})
					method := "executor.execute"
					if stream {
						method = "executor.execute_stream"
					}
					result, err := svc.Handle(method, jsonBytes(req))
					if err != nil {
						t.Fatal(err)
					}
					var final map[string]any
					if stream {
						select {
						case end := <-closed:
							if end["error"] != nil {
								t.Fatal(end)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("stream never closed")
						}
						if len(chunks) < 4 {
							t.Fatalf("host requires individual data events, got %d chunks", len(chunks))
						}
						for i, chunk := range chunks {
							if !bytes.HasPrefix(chunk, []byte("data: ")) {
								t.Fatalf("chunk %d is not a Codex data frame: %.80s", i, chunk)
							}
							var event map[string]any
							if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(chunk, []byte("data: "))), &event); err != nil {
								t.Fatal(err)
							}
							if event["sequence_number"] != float64(i) {
								t.Fatal("event order changed")
							}
							if event["type"] == terminal {
								final = objectValue(event["response"])
							}
						}
					} else {
						var envelope map[string]any
						if err := json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &envelope); err != nil {
							t.Fatal(err)
						}
						if envelope["type"] != terminal {
							t.Fatalf("missing Codex terminal envelope: %#v", envelope)
						}
						final = objectValue(envelope["response"])
					}
					if final == nil || final["status"] != status || objectValue(final["usage"])["total_tokens"] != float64(17) {
						t.Fatalf("lost terminal status or usage: %#v", final)
					}
					if tool {
						call := objectValue(final["output"].([]any)[0])
						if call["name"] != "echo" || call["arguments"] != `{"value":"fixture-value"}` {
							t.Fatalf("lost translated tool: %#v", call)
						}
					}
				})
			}
		}
	}
}

func TestCodexCapabilitiesAndTokenCountBoundary(t *testing.T) {
	capabilities := registration(defaultConfig())["capabilities"].(map[string]any)
	for _, key := range []string{"executor_input_formats", "executor_output_formats"} {
		formats := capabilities[key].([]string)
		if len(formats) != 2 || formats[0] != "openai-response" || formats[1] != "codex" {
			t.Fatalf("%s = %v", key, formats)
		}
	}
	if capabilities["request_interceptor"] == true {
		t.Fatal("obsolete Claude rejection hook is still registered")
	}
	svc := NewService()
	for _, format := range []string{"openai-response", "codex"} {
		_, err := svc.Handle("executor.count_tokens", jsonBytes(ExecutorRequest{Format: format}))
		api, ok := err.(*APIError)
		if !ok || api.Status != 400 || api.Kind != "unsupported_token_count" {
			t.Fatalf("unsupported token counting must not fabricate zero: %v", err)
		}
	}
}
