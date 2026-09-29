package basispoints

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
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
			if len(part) != 2 || part["type"] != "input_image" || part["file_id"] != "file-http-test" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"message":"Invalid request body."}`)
				return
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
	setLocalImageHTTPHost(service, server)
	request := imageRequest(map[string]any{"type": "input_image", "image_url": dataURL, "detail": "high"})
	result, err := service.Handle("executor.execute", jsonBytes(request))
	if err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || responses != 1 || !strings.Contains(string(result.(map[string]any)["Payload"].([]byte)), "local protocol fixture") {
		t.Fatal("local executor HTTP flow did not complete")
	}
}

func setLocalImageHTTPHost(service *Service, server *httptest.Server) {
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
}

// 服务端夹具仅按 issue #15 报告的格式白名单校验，不代表真实上游现场复现。
func TestNineImageHistoryUsesSupportedUploadedFilenames(t *testing.T) {
	dataURL, pngData := testImageDataURL(t)
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	responses := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/attachments":
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			part, err := reader.NextPart()
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(part)
			if err != nil || (!bytes.Equal(data, pngData) && !bytes.Equal(data, jpegData.Bytes())) {
				t.Error("uploaded image bytes changed")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fileID := fmt.Sprintf("file-local-%d", len(files))
			files[fileID] = part.FileName()
			_ = json.NewEncoder(w).Encode(map[string]any{"openai_file_id": fileID})
		case "/api/responses":
			responses++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			parts := lastUserContent(body)
			if len(parts) != 9 {
				t.Error("image history was dropped")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, value := range parts {
				part := objectValue(value)
				filename, exists := files[stringValue(part["file_id"])]
				if !exists {
					t.Error("response references a file that was not uploaded")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				extension := path.Ext(filename)
				switch extension {
				case ".jpeg", ".jpg", ".png", ".gif", ".webp":
				default:
					if extension == "" {
						extension = "none"
					}
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"message": "Invalid input: Expected image type to be a supported format: .jpeg, .jpg, .png, .gif, .webp but got " + extension})
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp-nine-images", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "nine-image fixture"}}}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	service := newHTTPTestService()
	service.cfg.ResponsesURL = server.URL + "/api/responses"
	setLocalImageHTTPHost(service, server)
	var parts []any
	for i := 0; i < 8; i++ {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": dataURL})
	}
	parts = append(parts, map[string]any{"type": "input_image", "image_url": "data:image/jpg;base64," + base64.StdEncoding.EncodeToString(jpegData.Bytes())})
	request := imageRequest(parts...)
	request.SourceFormat = "codex"
	request.OriginalRequest = imageRequest(map[string]any{"type": "input_text", "text": `C:\visualizations\result.png`}).Payload
	for i := 0; i < 2; i++ {
		result, err := service.Handle("executor.execute", jsonBytes(request))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(result.(map[string]any)["Payload"].([]byte)), "nine-image fixture") {
			t.Fatal("nine-image HTTP fixture did not complete")
		}
	}
	if len(files) != 2 || responses != 2 {
		t.Fatalf("uploads=%d responses=%d; expected cache reuse across history replay", len(files), responses)
	}
}
