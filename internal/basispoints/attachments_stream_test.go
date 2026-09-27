package basispoints

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestUploadedImageCompletesStreamThroughHostJSON(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	service := newHTTPTestService()
	uploads, upstreamCloses := 0, 0
	var emitted []byte
	var closeError string
	closed := make(chan struct{})
	service.SetHost(func(method string, payload any, out any) error {
		var request map[string]json.RawMessage
		if err := json.Unmarshal(jsonBytes(payload), &request); err != nil {
			return err
		}
		var result any
		switch method {
		case "host.http.do":
			uploads++
			result = map[string]any{"StatusCode": 201, "Headers": map[string][]string{}, "Body": jsonBytes(map[string]any{"openai_file_id": "file-stream-json"})}
		case "host.http.do_stream":
			var raw []byte
			if err := json.Unmarshal(request["body"], &raw); err != nil {
				return err
			}
			body, err := rawObject(raw)
			if err != nil {
				return err
			}
			part := objectValue(lastUserContent(body)[0])
			if part["file_id"] != "file-stream-json" || part["image_url"] != nil {
				return fmt.Errorf("stream request lost image reference")
			}
			result = map[string]any{"status_code": 200, "stream_id": "json-upstream", "headers": map[string][]string{"Content-Type": {"text/event-stream"}}}
		case "host.http.stream_read":
			response := map[string]any{"id": "resp-image-json", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "fixture image answer"}}}}}
			event := jsonBytes(map[string]any{"type": "response.completed", "response": response})
			raw := append([]byte("data: "), event...)
			raw = append(raw, 10, 10)
			result = map[string]any{"payload": raw, "done": true}
		case "host.http.stream_close":
			upstreamCloses++
		case "host.stream.emit":
			if err := json.Unmarshal(request["payload"], &emitted); err != nil {
				return err
			}
		case "host.stream.close":
			if value, exists := request["error"]; exists {
				_ = json.Unmarshal(value, &closeError)
			}
			close(closed)
		default:
			return fmt.Errorf("unexpected host callback %s", method)
		}
		if out != nil {
			return json.Unmarshal(jsonBytes(result), out)
		}
		return nil
	})
	request := imageRequest(map[string]any{"type": "input_image", "image_url": dataURL, "detail": "high"})
	request.Stream, request.StreamID = true, "downstream-json"
	if _, err := service.Handle("executor.execute_stream", jsonBytes(request)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("image stream did not terminate")
	}
	if uploads != 1 || upstreamCloses != 1 || closeError != "" || !strings.Contains(string(emitted), "response.completed") || !strings.Contains(string(emitted), "fixture image answer") {
		t.Fatalf("image stream incomplete: uploads=%d closed=%d error=%s", uploads, upstreamCloses, closeError)
	}
	if _, _, err := service.prepareRequest(request); err != nil {
		t.Fatal(err)
	}
	if uploads != 1 {
		t.Fatal("successful upload was not cached")
	}
}
