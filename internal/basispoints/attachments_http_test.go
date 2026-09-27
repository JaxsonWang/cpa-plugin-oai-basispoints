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
)

// 真实本地 HTTP 传输，响应为协议测试夹具，不用于证明远端识图成功。
func TestExecuteImageThroughLocalHTTP(t *testing.T) {
	dataURL, imageBytes := testImageDataURL(t)
	uploads, responses := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("ChatGPT-Account-ID") != "test-account" {
			t.Error("HTTP request authentication changed")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/attachments":
			uploads++
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			part, err := reader.NextPart()
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			content, err := io.ReadAll(part)
			if err != nil || part.FormName() != "file" || !bytes.Equal(content, imageBytes) {
				t.Error("uploaded image differs from input bytes")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": "file-http-test"})
		case "/api/responses":
			responses++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			part := objectValue(lastUserContent(body)[0])
			if part["file_id"] != "file-http-test" || part["image_url"] != nil || part["detail"] != "high" {
				t.Error("HTTP response request lost the attachment reference or detail")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp-local-test", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "local protocol fixture"}}}}})
		default:
			t.Error("unexpected HTTP route")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	service := newHTTPTestService()
	service.cfg.ResponsesURL = server.URL + "/api/responses"
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.http.do" {
			return fmt.Errorf("unexpected callback %s", method)
		}
		var wire struct {
			Method  string
			URL     string
			Headers http.Header
			Body    []byte
		}
		if err := json.Unmarshal(jsonBytes(payload), &wire); err != nil {
			return err
		}
		request, err := http.NewRequest(wire.Method, wire.URL, bytes.NewReader(wire.Body))
		if err != nil {
			return err
		}
		request.Header = wire.Headers
		response, err := server.Client().Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		// 模拟宿主真实 JSON 返回，而非跳过反序列化直接赋值。
		return json.Unmarshal(jsonBytes(map[string]any{"StatusCode": response.StatusCode, "Headers": response.Header, "Body": data}), out)
	})
	request := imageRequest(map[string]any{"type": "input_image", "image_url": dataURL, "detail": "high"})
	result, err := service.Handle("executor.execute", jsonBytes(request))
	if err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || responses != 1 || !strings.Contains(string(result.(map[string]any)["Payload"].([]byte)), "local protocol fixture") {
		t.Fatal("local executor HTTP flow did not complete")
	}
}
