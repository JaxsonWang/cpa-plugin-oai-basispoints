package basispoints

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIncrementalDeliveryPreservesByteSplitLifecycle(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, status := range []string{"completed", "incomplete"} {
			t.Run(format+"/"+status, func(t *testing.T) {
				message := map[string]any{"type": "message", "id": "msg_parts", "role": "assistant", "status": status, "phase": "final_answer", "content": []any{
					map[string]any{"type": "output_text", "text": "  汉字\r\n", "annotations": []any{map[string]any{"type": "file_citation", "file_id": "file_fixture", "filename": "fixture.txt", "index": 0}}},
					map[string]any{"type": "refusal", "refusal": "fixture refusal"},
					map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
					map[string]any{"type": "output_text", "text": "tail  ", "annotations": []any{}, "logprobs": []any{map[string]any{"token": "tail", "logprob": -0.1}}},
				}}
				response := map[string]any{"id": "resp_parts", "status": status, "output": []any{map[string]any{"type": "reasoning", "id": "rs_fixture", "summary": []any{}, "encrypted_content": "opaque-fixture"}, message}, "usage": map[string]any{"total_tokens": 17}}
				if status == "incomplete" {
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				}
				var frames [][]byte
				starts := 0
				d := newStreamDelivery(format, func() { starts++ }, func(frame []byte) error { frames = append(frames, bytes.Clone(frame)); return nil })
				decoder := newSSEDecoder()
				for _, b := range syntheticStream(response) {
					if err := decoder.feed([]byte{b}, func(kind, data string) error {
						if kind == "response."+status && (!d.committed || len(frames) == 0) {
							return fmt.Errorf("text was not delivered before the terminal event")
						}
						return d.consume(kind, data)
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := d.validateFinal(response); err != nil {
					t.Fatal(err)
				}
				if err := d.finish(response); err != nil {
					t.Fatal(err)
				}
				events := decodeIncrementalFrames(t, format, frames)
				var text string
				partAdds, partDones, terminal := 0, 0, 0
				for _, event := range events {
					switch event["type"] {
					case "response.output_text.delta":
						text += event["delta"].(string)
						if event["output_index"] != float64(1) {
							t.Fatal("output index changed")
						}
					case "response.content_part.added":
						partAdds++
					case "response.content_part.done":
						partDones++
					case "response.completed", "response.incomplete":
						terminal++
						if !bytes.Equal(jsonBytes(event["response"]), jsonBytes(response)) {
							t.Fatal("terminal fields changed")
						}
					}
				}
				if starts != 1 || text != "  汉字\r\ntail  " || partAdds != 4 || partDones != 4 || terminal != 1 {
					t.Fatalf("starts=%d text=%q parts=%d/%d terminal=%d", starts, text, partAdds, partDones, terminal)
				}
			})
		}
	}
}

func TestStreamingClientDisconnectAndQuiesceCloseUpstream(t *testing.T) {
	for _, mode := range []string{"disconnect", "quiesce"} {
		t.Run(mode, func(t *testing.T) {
			svc := NewService()
			firstText, upstreamClosed, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var firstOnce, closeOnce sync.Once
			reads, closes := 0, 0
			svc.SetHost(func(method string, payload any, out any) error {
				switch method {
				case "host.http.do_stream":
					*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "cancelable"}
				case "host.http.stream_read":
					reads++
					if reads == 1 {
						*out.(*streamChunk) = streamChunk{Payload: incrementalPrefix("hello")}
					} else {
						select {
						case <-upstreamClosed:
						case <-time.After(5 * time.Second):
							return fmt.Errorf("upstream was not canceled")
						}
						*out.(*streamChunk) = streamChunk{Done: true}
					}
				case "host.http.stream_close":
					closes++
					closeOnce.Do(func() { close(upstreamClosed) })
				case "host.stream.emit":
					frame := payload.(map[string]any)["payload"].([]byte)
					if bytes.Contains(frame, []byte(`"type":"response.output_text.delta"`)) {
						firstOnce.Do(func() { close(firstText) })
						if mode == "disconnect" {
							return fmt.Errorf("client disconnected")
						}
					}
				case "host.stream.close":
					close(closed)
				default:
					return fmt.Errorf("unexpected callback %s", method)
				}
				return nil
			})
			raw := jsonBytes(ExecutorRequest{Model: DefaultModelID, Format: "openai-response", Stream: true, StreamID: "client", Payload: jsonBytes(map[string]any{"input": "hello"}), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}})
			if _, err := svc.Handle("executor.execute_stream", raw); err != nil {
				t.Fatal(err)
			}
			select {
			case <-firstText:
			case <-time.After(5 * time.Second):
				t.Fatal("no first text")
			}
			if mode == "quiesce" {
				if _, err := svc.Handle("plugin.quiesce", nil); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("client stream leaked")
			}
			svc.streamWG.Wait()
			if closes != 1 || (mode == "disconnect" && reads != 1) {
				t.Fatalf("closes=%d reads=%d", closes, reads)
			}
			if mode == "quiesce" {
				if _, err := svc.Handle("executor.execute_stream", raw); err == nil || !strings.Contains(err.Error(), "shut down") {
					t.Fatalf("quiesced service accepted work: %v", err)
				}
			}
		})
	}
}
