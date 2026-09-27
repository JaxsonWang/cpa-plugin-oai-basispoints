package basispoints

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func clientStreamEvents(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	ended := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if ended {
			t.Fatal("data after stream end")
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			ended = true
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if event["sequence_number"] != float64(len(events)) {
			t.Fatalf("out of order sequence: %#v", event)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !ended {
		t.Fatal("missing stream terminator")
	}
	return events
}

func TestNativeToolStreamCanBeConsumedWithoutTrimmingOrDuplication(t *testing.T) {
	for _, tc := range []struct{ kind, field, event, input string }{
		{"function_call", "arguments", "response.function_call_arguments", `{"code":"  原始输入\n"}`},
		{"custom_tool_call", "input", "response.custom_tool_call_input", "  原始输入\r\n\t\n"},
		{"custom_tool_call", "input", "response.custom_tool_call_input", ""},
	} {
		t.Run(tc.kind+"/"+tc.input, func(t *testing.T) {
			call := map[string]any{"type": tc.kind, "id": "fc_client_stream", "call_id": "call_client_stream", "name": "js", "namespace": "mcp__node_repl", tc.field: tc.input}
			if tc.kind == "function_call" {
				call["status"] = "completed"
			}
			response := map[string]any{"id": "resp_client_stream", "status": "completed", "output": []any{messageItem("assistant", "Running tool."), call}}
			before := string(jsonBytes(response))
			events := clientStreamEvents(t, syntheticStream(response))
			var accumulated string
			added, finalized, done, completed := 0, 0, 0, 0
			for _, event := range events {
				kind := stringValue(event["type"])
				switch kind {
				case "response.output_item.added", "response.output_item.done":
					item := objectValue(event["item"])
					if item["type"] != tc.kind {
						continue
					}
					if event["output_index"] != float64(1) || item["namespace"] != "mcp__node_repl" || item["name"] != "js" || item["call_id"] != call["call_id"] || item["id"] != call["id"] {
						t.Fatalf("tool identity lost: %#v", event)
					}
					if kind == "response.output_item.added" {
						added++
						accumulated = item[tc.field].(string)
						if accumulated != "" {
							t.Fatal("added item duplicated the input delta")
						}
					} else {
						done++
						if !reflect.DeepEqual(item, call) || accumulated != tc.input {
							t.Fatalf("completed tool differs: item=%#v accumulated=%q", item, accumulated)
						}
					}
				case tc.event + ".delta":
					if event["item_id"] != call["id"] || event["output_index"] != float64(1) {
						t.Fatalf("delta identity lost: %#v", event)
					}
					accumulated += event["delta"].(string)
				case tc.event + ".done":
					finalized++
					if event[tc.field] != tc.input || event["item_id"] != call["id"] || event["output_index"] != float64(1) {
						t.Fatalf("finalized input differs: %#v", event)
					}
				case "response.completed":
					completed++
					if !reflect.DeepEqual(event["response"], response) {
						t.Fatalf("final response differs: %#v", event)
					}
				}
			}
			if added != 1 || finalized != 1 || done != 1 || completed != 1 || string(jsonBytes(response)) != before {
				t.Fatalf("invalid lifecycle: added=%d finalized=%d done=%d completed=%d", added, finalized, done, completed)
			}
		})
	}
}

func TestExecutorNativeToolRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, toolType := range []string{"function", "custom"} {
			for _, original := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/type=%s/original=%t", stream, toolType, original), func(t *testing.T) {
					source := namespaceTestSource(toolType, "js", "mcp__node_repl")
					source["input"] = []any{messageItem("user", "Inspect an app.")}
					var args any = map[string]any{"code": "nodeRepl.write('ok')"}
					if toolType == "custom" {
						args = "  raw input\n"
					}
					native := namespaceTestNative(t.Name(), "mcp__node_repl.js", args)
					upstream := map[string]any{"id": "resp_native_roundtrip", "status": "completed", "output": []any{native}}
					upstreamSSE := []byte("event: response.completed\ndata: " + string(jsonBytes(map[string]any{"type": "response.completed", "response": upstream})) + "\n\n")
					service := newHTTPTestService()
					closed := make(chan map[string]any, 1)
					var emitted []byte
					service.SetHost(func(method string, payload any, out any) error {
						switch method {
						case "host.http.do", "host.http.do_stream":
							var wire map[string]any
							if err := json.Unmarshal(payload.(map[string]any)["body"].([]byte), &wire); err != nil {
								return err
							}
							if _, present := wire["tools"]; present || !strings.Contains(string(jsonBytes(wire["input"])), "mcp__node_repl.js") {
								return fmt.Errorf("invalid upstream relay catalog")
							}
							if stream {
								*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "upstream-native-roundtrip"}
							} else {
								*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: jsonBytes(upstream)}
							}
						case "host.http.stream_read":
							*out.(*streamChunk) = streamChunk{Payload: upstreamSSE, Done: true}
						case "host.http.stream_close":
						case "host.stream.emit":
							emitted = payload.(map[string]any)["payload"].([]byte)
						case "host.stream.close":
							closed <- payload.(map[string]any)
						default:
							return fmt.Errorf("unexpected host method %s", method)
						}
						return nil
					})
					request := ExecutorRequest{Model: DefaultModelID, Payload: jsonBytes(source), Stream: stream, StreamID: "client-native-roundtrip", StorageJSON: jsonBytes(map[string]any{"access_token": "test-access", "account_id": "test-account"})}
					if original {
						request.OriginalRequest, request.Payload = request.Payload, []byte(`{"input":"not the original request"}`)
					}
					method := "executor.execute"
					if stream {
						method = "executor.execute_stream"
					}
					result, err := service.Handle(method, jsonBytes(request))
					if err != nil {
						t.Fatal(err)
					}
					var response map[string]any
					if stream {
						select {
						case closeResult := <-closed:
							if closeResult["error"] != nil {
								t.Fatalf("stream failed: %#v", closeResult)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("executor did not close stream")
						}
						for _, event := range clientStreamEvents(t, emitted) {
							if event["type"] == "response.completed" {
								response = objectValue(event["response"])
							}
						}
					} else if err := json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &response); err != nil {
						t.Fatal(err)
					}
					call := objectValue(response["output"].([]any)[0])
					if call["namespace"] != "mcp__node_repl" || call["name"] != "js" {
						t.Fatalf("executor lost tool routing: %#v", call)
					}
					image := []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,dGVzdA=="}}
					resultType := "function_call_output"
					if toolType == "custom" {
						resultType = "custom_tool_call_output"
					}
					source["input"] = append(source["input"].([]any), call, map[string]any{"type": resultType, "call_id": call["call_id"], "output": image})
					request.Payload, request.OriginalRequest = jsonBytes(source), nil
					prepared, _, err := service.prepareRequest(request)
					if err != nil {
						t.Fatal(err)
					}
					history := prepared["input"].([]any)
					if !reflect.DeepEqual(history[len(history)-2], native) || !reflect.DeepEqual(objectValue(history[len(history)-1])["output"], image) {
						t.Fatalf("next request lost native identity or image result: %#v", history[len(history)-2:])
					}
				})
			}
		}
	}
}
