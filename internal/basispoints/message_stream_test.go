package basispoints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMessageStreamPreservesContentLifecycle(t *testing.T) {
	for _, status := range []string{"completed", "incomplete"} {
		for _, tc := range []struct {
			name    string
			content []any
			indexes []int
			text    string
		}{
			{"pong", []any{map[string]any{"type": "output_text", "text": "pong", "annotations": []any{}}}, []int{0}, "pong"},
			{"mixed_parts", []any{
				map[string]any{"type": "output_text", "text": "  你好\r\n", "annotations": []any{map[string]any{"type": "file_citation", "file_id": "file_test", "filename": "test.txt", "index": 0}}},
				map[string]any{"type": "refusal", "refusal": "not visible text"},
				map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				map[string]any{"type": "output_text", "text": "again", "annotations": []any{}, "logprobs": []any{map[string]any{"token": "again", "logprob": -0.1, "top_logprobs": []any{}}}},
			}, []int{0, 3}, "  你好\r\nagain"},
			{"refusal_only", []any{map[string]any{"type": "refusal", "refusal": "declined"}}, nil, ""},
			{"empty_text", []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}, nil, ""},
			{"empty_content", []any{}, nil, ""},
		} {
			t.Run(status+"/"+tc.name, func(t *testing.T) {
				message := map[string]any{"id": "msg_visible", "type": "message", "role": "assistant", "status": status, "content": tc.content, "phase": "final_answer"}
				reasoning := map[string]any{"type": "reasoning", "id": "rs_hidden", "summary": []any{map[string]any{"type": "summary_text", "text": "not visible text"}}}
				response := map[string]any{"id": "resp_visible", "status": status, "output": []any{reasoning, message}}
				before := string(jsonBytes(response))
				var assembled []any
				var visible string
				var deltaIndexes []int
				var messageEvents []string
				added, done, terminal := 0, 0, 0
				partOpen, textDone := false, false
				for _, event := range clientStreamEvents(t, syntheticStream(response)) {
					kind := stringValue(event["type"])
					if strings.HasPrefix(kind, "response.reasoning") {
						t.Fatalf("reasoning unexpectedly replayed: %#v", event)
					}
					switch kind {
					case "response.output_item.added", "response.output_item.done":
						item := objectValue(event["item"])
						if item["id"] != message["id"] {
							continue
						}
						messageEvents = append(messageEvents, kind)
						if event["output_index"] != float64(1) {
							t.Fatalf("wrong output index: %#v", event)
						}
						if kind == "response.output_item.added" {
							added++
							assembled = item["content"].([]any)
							if len(assembled) != 0 || item["status"] != "in_progress" || item["phase"] != "final_answer" {
								t.Fatalf("message starts with completed content: %#v", item)
							}
						} else {
							done++
							if partOpen || string(jsonBytes(assembled)) != string(jsonBytes(tc.content)) || string(jsonBytes(item)) != string(jsonBytes(message)) {
								t.Fatalf("message differs: assembled=%#v item=%#v", assembled, item)
							}
						}
					case "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done":
						messageEvents = append(messageEvents, kind)
						if event["item_id"] != message["id"] || event["output_index"] != float64(1) || added != 1 || done != 0 {
							t.Fatalf("content identity or order lost: %#v", event)
						}
						index := int(event["content_index"].(float64))
						if kind == "response.content_part.added" {
							if partOpen || index != len(assembled) {
								t.Fatalf("content index compressed or reordered: %#v", event)
							}
							part := objectValue(event["part"])
							if part["type"] == "output_text" && part["text"] != "" {
								t.Fatal("initial text duplicates delta")
							}
							assembled = append(assembled, part)
							partOpen, textDone = true, false
							continue
						}
						if !partOpen || index != len(assembled)-1 {
							t.Fatalf("event outside content lifecycle: %#v", event)
						}
						part := objectValue(assembled[index])
						switch kind {
						case "response.output_text.delta":
							delta := event["delta"].(string)
							if textDone || part["type"] != "output_text" || delta == "" {
								t.Fatalf("invalid text delta: %#v", event)
							}
							part["text"] = part["text"].(string) + delta
							if logs, exists := part["logprobs"].([]any); exists {
								part["logprobs"] = append(logs, event["logprobs"].([]any)...)
							}
							visible += delta
							deltaIndexes = append(deltaIndexes, index)
							if expected, exists := objectValue(tc.content[index])["logprobs"]; exists && !reflect.DeepEqual(event["logprobs"], expected) {
								t.Fatal("text delta lost logprobs")
							}
						case "response.output_text.done":
							if textDone || part["type"] != "output_text" || event["text"] != part["text"] {
								t.Fatalf("text finalized incorrectly: %#v", event)
							}
							textDone = true
						case "response.content_part.done":
							if part["type"] == "output_text" && !textDone {
								t.Fatal("missing text done")
							}
							if string(jsonBytes(part)) != string(jsonBytes(event["part"])) {
								t.Fatalf("content differs: %#v", event)
							}
							partOpen = false
						}
					case "response.completed", "response.incomplete":
						terminal++
						if kind != "response."+status || string(jsonBytes(event["response"])) != before {
							t.Fatalf("terminal response changed: %#v", event)
						}
					}
				}
				if visible != tc.text || !reflect.DeepEqual(deltaIndexes, tc.indexes) || added != 1 || done != 1 || terminal != 1 || string(jsonBytes(response)) != before {
					t.Fatalf("invalid replay: text=%q indexes=%v added=%d done=%d terminal=%d", visible, deltaIndexes, added, done, terminal)
				}
				if tc.name == "pong" {
					want := []string{"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done"}
					if !reflect.DeepEqual(messageEvents, want) {
						t.Fatalf("message lifecycle: %v", messageEvents)
					}
				}
			})
		}
	}
}

func TestExecutorMessageTextReplay(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				response := map[string]any{"id": "resp_text", "status": "completed", "output": []any{map[string]any{"id": "msg_text", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "pong", "annotations": []any{}}}}}}
				var upstream strings.Builder
				writeSSE(&upstream, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_text", "output_index": 0, "content_index": 0, "delta": "pong"})
				writeSSE(&upstream, "response.completed", map[string]any{"type": "response.completed", "response": response})
				svc := NewService()
				closed := make(chan map[string]any, 1)
				var chunks [][]byte
				svc.SetHost(func(method string, payload any, out any) error {
					switch method {
					case "host.http.do":
						*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: jsonBytes(response)}
					case "host.http.do_stream":
						*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "upstream-text"}
					case "host.http.stream_read":
						*out.(*streamChunk) = streamChunk{Payload: []byte(upstream.String()), Done: true}
					case "host.http.stream_close":
					case "host.stream.emit":
						chunks = append(chunks, bytes.Clone(payload.(map[string]any)["payload"].([]byte)))
					case "host.stream.close":
						closed <- payload.(map[string]any)
					default:
						return fmt.Errorf("unexpected host method: %s", method)
					}
					return nil
				})
				req := ExecutorRequest{Model: DefaultModelID, Format: format, SourceFormat: format, Stream: stream, StreamID: "client-text", Payload: jsonBytes(map[string]any{"input": "只回复 pong"}), StorageJSON: jsonBytes(map[string]any{"access_token": "test-access", "account_id": "test-account"})}
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				result, err := svc.Handle(method, jsonBytes(req))
				if err != nil {
					t.Fatal(err)
				}
				if !stream {
					var got map[string]any
					if err := json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &got); err != nil {
						t.Fatal(err)
					}
					if format == "codex" {
						got = objectValue(got["response"])
					}
					if !reflect.DeepEqual(got, response) {
						t.Fatalf("nonstream response changed: %#v", got)
					}
					return
				}
				select {
				case result := <-closed:
					if result["error"] != nil {
						t.Fatal(result)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("stream did not close")
				}
				wire := bytes.Join(chunks, []byte{10, 10})
				if format == "codex" {
					wire = append(wire, []byte{10, 10}...)
					wire = append(wire, []byte("data: [DONE]\n\n")...)
				}
				var text string
				var final map[string]any
				for _, event := range clientStreamEvents(t, wire) {
					if event["type"] == "response.output_text.delta" {
						text += event["delta"].(string)
					}
					if event["type"] == "response.completed" {
						final = objectValue(event["response"])
					}
				}
				if text != "pong" || !reflect.DeepEqual(final, response) {
					t.Fatalf("stream text=%q; final=%#v", text, final)
				}
			})
		}
	}
}
