package basispoints

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func reasoningStreamItem(text string) map[string]any {
	return map[string]any{"id": "rs_live", "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": text}}, "encrypted_content": "opaque-fixture"}
}

func reasoningStreamPrefix(text string) []byte {
	return streamFixtureEvents(
		map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_incremental", "status": "in_progress", "output": []any{}}},
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "rs_live", "type": "reasoning", "summary": []any{}}},
		map[string]any{"type": "response.reasoning_summary_part.added", "output_index": 0, "item_id": "rs_live", "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}},
		map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "item_id": "rs_live", "summary_index": 0, "delta": text},
	)
}

func TestReasoningStreamDeliveryAndFinalReplay(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, mode := range []string{"complete", "partial", "item_only", "incomplete"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				text := "  推理\r\n完成  "
				reasoning := reasoningStreamItem(text)
				response := incrementalTerminal("answer")
				response["output"] = append([]any{reasoning}, response["output"].([]any)...)
				if mode == "incomplete" {
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				}
				var frames [][]byte
				starts := 0
				d := newStreamDelivery(format, func() { starts++ }, func(frame []byte) error {
					frames = append(frames, bytes.Clone(frame))
					return nil
				})
				feed := func(payload []byte) {
					t.Helper()
					decoder := newSSEDecoder()
					for _, b := range payload {
						if err := decoder.feed([]byte{b}, d.consume); err != nil {
							t.Fatal(err)
						}
					}
				}
				if mode == "item_only" {
					feed(streamFixtureEvents(
						map[string]any{"type": "response.created", "response": map[string]any{"id": response["id"]}},
						map[string]any{"type": "response.output_item.added", "output_index": 0, "item": reasoning},
						map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoning},
					))
					if d.committed || len(frames) != 0 {
						t.Fatal("reasoning lifecycle alone committed the stream")
					}
					feed(streamFixtureEvents(
						map[string]any{"type": "response.output_item.added", "output_index": 1, "item": response["output"].([]any)[1]},
						map[string]any{"type": "response.content_part.added", "output_index": 1, "content_index": 0, "item_id": "msg_incremental", "part": map[string]any{"type": "output_text", "text": ""}},
						map[string]any{"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "item_id": "msg_incremental", "delta": "answer"},
					))
					reasoningFrames := 0
					for _, event := range decodeIncrementalFrames(t, format, frames) {
						if objectValue(event["item"])["type"] == "reasoning" {
							reasoningFrames++
							if !bytes.Equal(jsonBytes(event["item"]), jsonBytes(reasoning)) {
								t.Fatal("pending reasoning item was rewritten as a message")
							}
						}
					}
					if reasoningFrames != 2 {
						t.Fatal("pending reasoning lifecycle waited for the final response")
					}
				} else {
					feed(reasoningStreamPrefix("  推理\r\n"))
					if !d.committed || len(frames) == 0 {
						t.Fatal("reasoning delta did not start delivery before any answer")
					}
					if mode == "complete" {
						feed(streamFixtureEvents(
							map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "item_id": "rs_live", "summary_index": 0, "delta": "完成  "},
							map[string]any{"type": "response.reasoning_summary_text.done", "output_index": 0, "item_id": "rs_live", "summary_index": 0, "text": text},
							map[string]any{"type": "response.reasoning_summary_part.done", "output_index": 0, "item_id": "rs_live", "summary_index": 0, "part": reasoning["summary"].([]any)[0]},
							map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoning},
						))
					}
				}
				if len(d.messages) != 0 && mode != "item_only" {
					t.Fatal("reasoning was registered as a message")
				}
				if err := d.validateFinal(response); err != nil {
					t.Fatal(err)
				}
				if err := d.finish(response); err != nil {
					t.Fatal(err)
				}
				events := decodeIncrementalFrames(t, format, frames)
				counts := map[string]int{}
				var summary, answer string
				for _, event := range events {
					kind := stringValue(event["type"])
					if kind == "response.output_item.added" || kind == "response.output_item.done" {
						if objectValue(event["item"])["type"] == "reasoning" {
							counts[kind]++
							if kind == "response.output_item.done" && !bytes.Equal(jsonBytes(event["item"]), jsonBytes(reasoning)) {
								t.Fatal("reasoning item or encrypted content changed")
							}
						}
					} else {
						counts[kind]++
					}
					if kind == "response.reasoning_summary_text.delta" {
						summary += event["delta"].(string)
					}
					if kind == "response.output_text.delta" {
						answer += event["delta"].(string)
					}
				}
				for _, kind := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response." + stringValue(response["status"])} {
					if counts[kind] != 1 {
						t.Fatalf("%s emitted %d times", kind, counts[kind])
					}
				}
				if mode != "item_only" {
					for _, kind := range []string{"response.reasoning_summary_part.added", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done"} {
						if counts[kind] != 1 {
							t.Fatalf("%s emitted %d times", kind, counts[kind])
						}
					}
					if summary != text {
						t.Fatalf("summary changed or repeated: %q", summary)
					}
				}
				if starts != 1 || answer != "answer" {
					t.Fatalf("starts=%d answer=%q", starts, answer)
				}
			})
		}
	}
}

func TestExecutorStreamsReasoningBeforeUpstreamCompletes(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		t.Run(format, func(t *testing.T) {
			allowFinal, firstSummary, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var release, summaryOnce sync.Once
			unblock := func() { release.Do(func() { close(allowFinal) }) }
			defer unblock()
			response := map[string]any{"id": "resp_incremental", "status": "completed", "output": []any{reasoningStreamItem("正在思考")}}
			var frames [][]byte
			reads := 0
			svc := newHTTPTestService()
			svc.SetHost(func(method string, payload any, out any) error {
				switch method {
				case "host.http.do_stream":
					*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "upstream-reasoning"}
				case "host.http.stream_read":
					reads++
					if reads == 1 {
						*out.(*streamChunk) = streamChunk{Payload: reasoningStreamPrefix("正在思考")}
					} else {
						select {
						case <-allowFinal:
						case <-time.After(5 * time.Second):
							return fmt.Errorf("test did not release the upstream terminal event")
						}
						*out.(*streamChunk) = streamChunk{Payload: streamFixtureEvents(map[string]any{"type": "response.completed", "response": response}), Done: true}
					}
				case "host.http.stream_close":
				case "host.stream.emit":
					frame := bytes.Clone(payload.(map[string]any)["payload"].([]byte))
					frames = append(frames, frame)
					if bytes.Contains(frame, []byte(`"type":"response.reasoning_summary_text.delta"`)) {
						summaryOnce.Do(func() { close(firstSummary) })
					}
				case "host.stream.close":
					close(closed)
				default:
					return fmt.Errorf("unexpected host callback %s", method)
				}
				return nil
			})
			returned := make(chan error, 1)
			go func() {
				_, err := svc.Handle("executor.execute_stream", jsonBytes(ExecutorRequest{Model: DefaultModelID, Format: format, SourceFormat: format, Stream: true, StreamID: "client-reasoning", Payload: jsonBytes(map[string]any{"model": DefaultModelID, "input": "hello"}), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}))
				returned <- err
			}()
			early := false
			select {
			case <-firstSummary:
				early = true
			case <-time.After(time.Second):
			}
			if early {
				select {
				case err := <-returned:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("executor headers waited for the terminal event")
				}
			}
			unblock()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("client stream did not close")
			}
			if !early {
				t.Fatal("first reasoning summary waited for the terminal event")
			}
			var summary strings.Builder
			for _, event := range decodeIncrementalFrames(t, format, frames) {
				if event["type"] == "error" {
					t.Fatal(event)
				}
				if event["type"] == "response.reasoning_summary_text.delta" {
					summary.WriteString(event["delta"].(string))
				}
			}
			if summary.String() != "正在思考" {
				t.Fatalf("summary changed or repeated: %q", summary.String())
			}
		})
	}
}

func TestReasoningStreamCommitBoundaries(t *testing.T) {
	for _, mode := range []string{"empty_delta", "no_metadata", "missing_item", "missing_part", "wrong_item", "wrong_part_type", "negative_index"} {
		t.Run(mode, func(t *testing.T) {
			writes, starts := 0, 0
			d := newStreamDelivery("codex", func() { starts++ }, func([]byte) error { writes++; return nil })
			err := newSSEDecoder().feed(reasoningStreamPrefix("summary"), func(kind, data string) error {
				value, _ := parseRelayObject(data)
				if (mode == "no_metadata" && kind == "response.created") ||
					(mode == "missing_item" && kind == "response.output_item.added") ||
					(mode == "missing_part" && kind == "response.reasoning_summary_part.added") {
					return nil
				}
				if mode == "wrong_part_type" && kind == "response.reasoning_summary_part.added" {
					objectValue(value["part"])["type"] = "output_text"
				}
				if kind == "response.reasoning_summary_text.delta" {
					switch mode {
					case "empty_delta":
						value["delta"] = ""
					case "wrong_item":
						value["item_id"] = "rs_wrong"
					case "negative_index":
						value["summary_index"] = -1
					}
				}
				return d.consume(kind, string(jsonBytes(value)))
			})
			wantError := mode != "empty_delta" && mode != "no_metadata"
			if (err != nil) != wantError || d.committed || writes != 0 || starts != 0 {
				t.Fatalf("err=%v committed=%t writes=%d starts=%d", err, d.committed, writes, starts)
			}
		})
	}
}

func TestReasoningStreamRejectsInconsistentEvents(t *testing.T) {
	for _, event := range []map[string]any{
		{"type": "response.reasoning_summary_text.done", "output_index": 0, "item_id": "rs_live", "summary_index": 0, "text": "changed"},
		{"type": "response.reasoning_summary_part.done", "output_index": 0, "item_id": "rs_live", "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": "summary"}},
		{"type": "response.reasoning_summary_text.delta", "output_index": 0, "item_id": "rs_wrong", "summary_index": 0, "delta": "more"},
		{"type": "response.reasoning_summary_part.added", "output_index": 0, "item_id": "rs_live", "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_conflict"}},
	} {
		t.Run(stringValue(event["type"]), func(t *testing.T) {
			d := newStreamDelivery("codex", func() {}, func([]byte) error { return nil })
			if err := newSSEDecoder().feed(reasoningStreamPrefix("summary"), d.consume); err != nil {
				t.Fatal(err)
			}
			if err := newSSEDecoder().feed(streamFixtureEvents(event), d.consume); err == nil {
				t.Fatal("accepted inconsistent reasoning event")
			}
		})
	}
}

func TestReasoningStreamMultipleItemsAndSummaries(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, lead := range []string{"reasoning", "message"} {
			t.Run(format+"/"+lead, func(t *testing.T) {
				reasoning := reasoningStreamItem("first")
				reasoning["summary"] = append(reasoning["summary"].([]any), map[string]any{"type": "summary_text", "text": ""}, map[string]any{"type": "summary_text", "text": "  第二段\r\n"})
				other := reasoningStreamItem("tail")
				other["id"] = "rs_other"
				response := incrementalTerminal("answer")
				message := response["output"].([]any)[0]
				response["output"] = []any{reasoning, message, other}
				if lead == "message" {
					response["output"] = []any{message, reasoning, other}
				}
				var frames [][]byte
				d := newStreamDelivery(format, func() {}, func(frame []byte) error { frames = append(frames, bytes.Clone(frame)); return nil })
				before := string(jsonBytes(response))
				if err := newSSEDecoder().feed(syntheticStream(response), d.consume); err != nil {
					t.Fatal(err)
				}
				if err := d.validateFinal(response); err != nil {
					t.Fatal(err)
				}
				if err := d.finish(response); err != nil {
					t.Fatal(err)
				}
				counts := map[string]int{}
				var summary string
				for _, event := range decodeIncrementalFrames(t, format, frames) {
					kind := stringValue(event["type"])
					counts[kind]++
					if kind == "response.reasoning_summary_text.delta" {
						summary += event["delta"].(string)
					}
				}
				if summary != "first  第二段\r\ntail" || counts["response.reasoning_summary_part.added"] != 4 || counts["response.reasoning_summary_part.done"] != 4 || counts["response.reasoning_summary_text.done"] != 4 || counts["response.output_item.added"] != 3 || counts["response.output_item.done"] != 3 || before != string(jsonBytes(response)) {
					t.Fatalf("summary=%q counts=%v", summary, counts)
				}
			})
		}
	}
}

func TestReasoningStreamRejectsChangedFinalItem(t *testing.T) {
	for _, mode := range []string{"missing_item", "wrong_id", "wrong_type", "missing_summary", "changed_text", "extra_completed_summary"} {
		t.Run(mode, func(t *testing.T) {
			d := newStreamDelivery("codex", func() {}, func([]byte) error { return nil })
			response := map[string]any{"id": "resp_incremental", "status": "completed", "output": []any{reasoningStreamItem("summary")}}
			if err := newSSEDecoder().feed(syntheticStream(response), d.consume); err != nil {
				t.Fatal(err)
			}
			item := objectValue(response["output"].([]any)[0])
			switch mode {
			case "missing_item":
				response["output"] = []any{}
			case "wrong_id":
				item["id"] = "rs_wrong"
			case "wrong_type":
				item["type"] = "message"
			case "missing_summary":
				item["summary"] = []any{}
			case "changed_text":
				objectValue(item["summary"].([]any)[0])["text"] = "changed"
			case "extra_completed_summary":
				item["summary"] = append(item["summary"].([]any), map[string]any{"type": "summary_text", "text": "extra"})
			}
			if err := d.validateFinal(response); err == nil {
				t.Fatal("accepted a final response inconsistent with delivered reasoning")
			}
		})
	}
}
