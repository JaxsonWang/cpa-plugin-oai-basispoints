package basispoints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func streamFixtureEvents(events ...map[string]any) []byte {
	var b strings.Builder
	for _, event := range events {
		writeSSE(&b, stringValue(event["type"]), event)
	}
	return []byte(b.String())
}

func incrementalPrefix(text string) []byte {
	return streamFixtureEvents(
		map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_incremental", "status": "in_progress", "output": []any{}}},
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_incremental", "role": "assistant", "status": "in_progress", "content": []any{}}},
		map[string]any{"type": "response.content_part.added", "output_index": 0, "content_index": 0, "item_id": "msg_incremental", "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_incremental", "delta": text},
	)
}

func incrementalTerminal(text string, tail ...any) map[string]any {
	message := map[string]any{"type": "message", "id": "msg_incremental", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	return map[string]any{"id": "resp_incremental", "status": "completed", "output": append([]any{message}, tail...), "usage": map[string]any{"total_tokens": 17}}
}

func decodeIncrementalFrames(t *testing.T, format string, frames [][]byte) []map[string]any {
	t.Helper()
	wire := bytes.Join(frames, nil)
	if format == "codex" {
		wire = append(bytes.Join(frames, []byte("\n\n")), []byte("\n\n")...)
	}
	var events []map[string]any
	ended := false
	if err := newSSEDecoder().feed(wire, func(_ string, data string) error {
		if data == "[DONE]" {
			ended = true
			return nil
		}
		if ended {
			return fmt.Errorf("event after DONE")
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		if event["sequence_number"] != float64(len(events)) {
			return fmt.Errorf("non-monotonic sequence")
		}
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestIncrementalFailureDoesNotRetryOrDeliverPartialToolBatch(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, mode := range []string{"invalid_tool", "upstream_failure", "transport_failure", "truncated", "changed_text", "size_limit", "incomplete_tool", "valid_tools"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				good, patch := relayFixture(t.Name()+"-good", false)
				bad, _ := relayFixture(t.Name()+"-bad", true)
				text := "  你好\r\n"
				response := incrementalTerminal(text, good, bad)
				switch mode {
				case "valid_tools":
					response = incrementalTerminal(text, good)
				case "changed_text":
					response = incrementalTerminal("different", good)
				case "incomplete_tool":
					response["status"] = "incomplete"
				}
				last := streamChunk{Payload: streamFixtureEvents(map[string]any{"type": "response." + stringValue(response["status"]), "response": response}), Done: true}
				switch mode {
				case "upstream_failure":
					last.Payload = streamFixtureEvents(map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"message": "private-upstream-message"}}})
				case "transport_failure":
					last.Payload, last.Error = nil, "connection reset"
				case "truncated":
					last.Payload = nil
				case "size_limit":
					last.Payload = bytes.Repeat([]byte("x"), 4000)
				}
				svc := NewService()
				if mode == "size_limit" {
					svc.cfg.MaxResponseBytes = 3000
				}
				var mu sync.Mutex
				var frames [][]byte
				reads, attempts, upstreamCloses := 0, 0, 0
				closed := make(chan map[string]any, 1)
				svc.SetHost(func(method string, payload any, out any) error {
					switch method {
					case "host.http.do_stream":
						attempts++
						*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "upstream"}
					case "host.http.stream_read":
						reads++
						if reads == 1 {
							*out.(*streamChunk) = streamChunk{Payload: incrementalPrefix(text)}
						} else {
							*out.(*streamChunk) = last
						}
					case "host.http.stream_close":
						upstreamCloses++
					case "host.stream.emit":
						mu.Lock()
						frames = append(frames, bytes.Clone(payload.(map[string]any)["payload"].([]byte)))
						mu.Unlock()
					case "host.stream.close":
						closed <- payload.(map[string]any)
					default:
						return fmt.Errorf("unexpected host callback %s", method)
					}
					return nil
				})
				source := map[string]any{"model": DefaultModelID, "input": "hello", "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}, map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}}}}
				_, err := svc.Handle("executor.execute_stream", jsonBytes(ExecutorRequest{Model: DefaultModelID, Format: format, SourceFormat: format, Stream: true, StreamID: "client", Payload: jsonBytes(source), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}))
				if err != nil {
					t.Fatal(err)
				}
				select {
				case result := <-closed:
					if result["error"] != nil {
						t.Fatal("stream failure would become an untyped host error", result)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("stream did not close")
				}
				if attempts != 1 || upstreamCloses != 1 {
					t.Fatalf("attempts=%d closes=%d", attempts, upstreamCloses)
				}
				mu.Lock()
				events := decodeIncrementalFrames(t, format, frames)
				mu.Unlock()
				var actual string
				tools, failures, terminals := 0, 0, 0
				for _, event := range events {
					if event["type"] == "response.output_text.delta" {
						actual += event["delta"].(string)
					}
					if event["type"] == "response.output_item.added" {
						kind := objectValue(event["item"])["type"]
						if kind == "function_call" || kind == "custom_tool_call" {
							tools++
						}
					}
					if event["type"] == "error" {
						failures++
						if strings.Contains(string(jsonBytes(event)), "private-upstream-message") {
							t.Fatal("leaked upstream failure body")
						}
					}
					if event["type"] == "response.completed" || event["type"] == "response.incomplete" {
						terminals++
						if mode == "valid_tools" {
							output := objectValue(event["response"])["output"].([]any)
							if objectValue(output[1])["input"] != patch {
								t.Fatal("changed raw patch input")
							}
						}
					}
				}
				if actual != text {
					t.Fatalf("text changed or repeated: %q", actual)
				}
				if mode == "valid_tools" {
					if tools != 1 || failures != 0 || terminals != 1 {
						t.Fatalf("tools=%d failures=%d terminals=%d", tools, failures, terminals)
					}
				} else if tools != 0 || failures != 1 || terminals != 0 || rememberedNativeCall(stringValue(good["call_id"])) != nil {
					t.Fatalf("failed stream leaked tools or success: tools=%d failures=%d terminals=%d", tools, failures, terminals)
				}
			})
		}
	}
}

func TestExecutorStreamsTextBeforeUpstreamCompletes(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		t.Run(format, func(t *testing.T) {
			message := map[string]any{"type": "message", "id": "msg_live", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "你好  world", "annotations": []any{}}}}
			terminal := map[string]any{"id": "resp_live", "status": "completed", "output": []any{message}, "usage": map[string]any{"total_tokens": 17}}
			first := streamFixtureEvents(
				map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_live", "status": "in_progress", "output": []any{}}},
				map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_live", "role": "assistant", "status": "in_progress", "content": []any{}}},
				map[string]any{"type": "response.content_part.added", "output_index": 0, "content_index": 0, "item_id": "msg_live", "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
				map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_live", "delta": "你好  "},
			)
			last := streamFixtureEvents(
				map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_live", "delta": "world"},
				map[string]any{"type": "response.completed", "response": terminal},
			)
			allowFinal, firstText, closed := make(chan struct{}), make(chan struct{}), make(chan map[string]any, 1)
			var release, textOnce sync.Once
			unblock := func() { release.Do(func() { close(allowFinal) }) }
			defer unblock()
			var mu sync.Mutex
			var frames [][]byte
			reads, upstreamCloses := 0, 0
			svc := NewService()
			svc.SetHost(func(method string, payload any, out any) error {
				switch method {
				case "host.http.do_stream":
					*out.(*upstreamStream) = upstreamStream{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "upstream-live"}
				case "host.http.stream_read":
					reads++
					if reads == 1 {
						*out.(*streamChunk) = streamChunk{Payload: first}
					} else {
						select {
						case <-allowFinal:
						case <-time.After(5 * time.Second):
							return fmt.Errorf("test did not release the upstream terminal event")
						}
						*out.(*streamChunk) = streamChunk{Payload: last, Done: true}
					}
				case "host.http.stream_close":
					mu.Lock()
					upstreamCloses++
					mu.Unlock()
				case "host.stream.emit":
					frame := bytes.Clone(payload.(map[string]any)["payload"].([]byte))
					mu.Lock()
					frames = append(frames, frame)
					mu.Unlock()
					if bytes.Contains(frame, []byte(`"type":"response.output_text.delta"`)) {
						textOnce.Do(func() { close(firstText) })
					}
				case "host.stream.close":
					closed <- payload.(map[string]any)
				default:
					return fmt.Errorf("unexpected host method %s", method)
				}
				return nil
			})
			returned := make(chan error, 1)
			go func() {
				_, err := svc.Handle("executor.execute_stream", jsonBytes(ExecutorRequest{Model: DefaultModelID, Format: format, SourceFormat: format, Stream: true, StreamID: "client-live", Payload: jsonBytes(map[string]any{"model": DefaultModelID, "input": "hello"}), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}))
				returned <- err
			}()
			early := false
			select {
			case <-firstText:
				early = true
			case <-time.After(time.Second):
			}
			unblock()
			select {
			case err := <-returned:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("executor did not return")
			}
			select {
			case result := <-closed:
				if result["error"] != nil {
					t.Fatal(result)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("client stream was not closed")
			}
			if !early {
				t.Fatal("first text waited for the upstream terminal event")
			}
			mu.Lock()
			defer mu.Unlock()
			wire := bytes.Join(frames, nil)
			if format == "codex" {
				wire = append(bytes.Join(frames, []byte("\n\n")), []byte("\n\ndata: [DONE]\n\n")...)
			}
			var text string
			deltas, terminals := 0, 0
			for _, event := range clientStreamEvents(t, wire) {
				if event["type"] == "response.output_text.delta" {
					text += event["delta"].(string)
					deltas++
				}
				if event["type"] == "response.completed" {
					terminals++
				}
			}
			if text != "你好  world" || deltas != 2 || terminals != 1 || upstreamCloses != 1 {
				t.Fatalf("text=%q deltas=%d terminals=%d upstream_closes=%d", text, deltas, terminals, upstreamCloses)
			}
		})
	}
}
