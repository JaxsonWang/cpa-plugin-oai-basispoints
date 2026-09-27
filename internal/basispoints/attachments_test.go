package basispoints

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testImageDataURL(t *testing.T) (string, []byte) {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes()), data.Bytes()
}

func imageRequest(parts ...any) ExecutorRequest {
	return ExecutorRequest{
		Model: DefaultModelID, HostCallbackID: "test-callback",
		StorageJSON: jsonBytes(map[string]any{"access_token": "test-access", "account_id": "test-account"}),
		Payload: jsonBytes(map[string]any{
			"input":     []any{map[string]any{"role": "user", "content": parts}},
			"reasoning": map[string]any{"effort": "xhigh"},
		}),
	}
}

func lastUserContent(body map[string]any) []any {
	items := body["input"].([]any)
	return objectValue(items[len(items)-1])["content"].([]any)
}

func assertImageUpload(t *testing.T, payload any, wantBytes []byte) {
	t.Helper()
	var wire struct {
		Method  string      `json:"method"`
		URL     string      `json:"url"`
		Headers http.Header `json:"headers"`
		Body    []byte      `json:"body"`
		ID      string      `json:"host_callback_id"`
	}
	if err := json.Unmarshal(jsonBytes(payload), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ID != "test-callback" || wire.Method != http.MethodPost || wire.Headers.Get("Authorization") != "Bearer test-access" {
		t.Fatal("incorrect upload scope or credentials")
	}
	if wire.URL != "https://bps.openai.com/basispoints/api/attachments" {
		t.Fatal("incorrect upload endpoint")
	}
	mediaType, params, err := mime.ParseMediaType(wire.Headers.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatal("invalid multipart header")
	}
	reader := multipart.NewReader(bytes.NewReader(wire.Body), params["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(part)
	if err != nil || !bytes.Equal(data, wantBytes) || part.FormName() != "file" || part.FileName() != "image.png" || part.Header.Get("Content-Type") != "image/png" {
		t.Fatal("multipart file differs from official contract")
	}
	if _, err := reader.NextPart(); err != io.EOF {
		t.Fatal("unexpected upload field")
	}
}

func TestInlineImageUploadWireContract(t *testing.T) {
	dataURL, imageBytes := testImageDataURL(t)
	for _, stream := range []bool{false, true} {
		for _, original := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/original=%t", stream, original), func(t *testing.T) {
				service := newHTTPTestService()
				uploads, responses := 0, 0
				var received map[string]any
				service.SetHost(func(method string, payload any, out any) error {
					wire := payload.(map[string]any)
					if strings.HasSuffix(wire["url"].(string), "/attachments") {
						uploads++
						if method != "host.http.do" {
							t.Fatal("incorrect upload transport")
						}
						assertImageUpload(t, payload, imageBytes)
						*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-uploaded"})}
						return nil
					}
					responses++
					if err := json.Unmarshal(wire["body"].([]byte), &received); err != nil {
						return err
					}
					if stream {
						if method != "host.http.do_stream" {
							t.Fatal("stream transport lost")
						}
						*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "test-stream"}
					} else {
						if method != "host.http.do" {
							t.Fatal("incorrect response transport")
						}
						*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200}
					}
					return nil
				})
				request := imageRequest(map[string]any{"type": "input_image", "image_url": dataURL, "detail": "high"}, map[string]any{"type": "input_image", "image_url": dataURL})
				request.Stream = stream
				sourceRaw := request.Payload
				if original {
					request.OriginalRequest = sourceRaw
					request.Payload = jsonBytes(map[string]any{"input": "wrong source"})
				}
				body, credential, err := service.prepareRequest(request)
				if err != nil {
					t.Fatal(err)
				}
				source, _ := rawObject(sourceRaw)
				before, _ := prepareResponsesBody(source, service.config())
				if !reflect.DeepEqual(body["metadata"], before["metadata"]) {
					t.Fatal("upload changed task or turn metadata")
				}
				if stream {
					_, err = service.upstreamStream(request, body, credential)
				} else {
					_, err = service.upstreamRequest(request, body, credential, false)
				}
				if err != nil {
					t.Fatal(err)
				}
				parts := lastUserContent(received)
				if len(parts) != 2 || received["reasoning_effort"] != "xhigh" || received["stream"] != stream {
					t.Fatal("request content or settings changed")
				}
				for i, detail := range []string{"high", "auto"} {
					if !reflect.DeepEqual(objectValue(parts[i]), map[string]any{"type": "input_image", "file_id": "file-uploaded", "detail": detail}) {
						t.Fatal("incorrect image reference")
					}
				}
				if _, _, err := service.prepareRequest(request); err != nil {
					t.Fatal(err)
				}
				if uploads != 1 || responses != 1 {
					t.Fatalf("uploads=%d responses=%d", uploads, responses)
				}
			})
		}
	}
}

func TestAttachmentFailureStopsResponseSubmission(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	cases := []struct {
		name   string
		status int
		body   any
		err    error
		want   int
	}{
		{"unauthorized", 401, "expired", nil, 401},
		{"oversize", 413, "too large", nil, 413},
		{"invalid", 422, "Invalid file", nil, 422},
		{"rate-limit", 429, "rate limited", nil, 429},
		{"server", 503, "unavailable", nil, 503},
		{"no-id", 200, map[string]any{}, nil, 502},
		{"wrong-field", 200, map[string]any{"id": "wrong-field"}, nil, 502},
		{"blank-id", 200, map[string]any{"openai_file_id": " "}, nil, 502},
		{"wrong-type", 200, map[string]any{"openai_file_id": 123}, nil, 502},
		{"non-object", 200, "not an attachment", nil, 502},
		{"transport", 0, nil, errors.New("interrupted"), 502},
	}
	for _, tc := range cases {
		for _, method := range []string{"executor.execute", "executor.execute_stream"} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				service := newHTTPTestService()
				calls := 0
				service.SetHost(func(method string, payload any, out any) error {
					calls++
					if method != "host.http.do" || !strings.HasSuffix(payload.(map[string]any)["url"].(string), "/attachments") {
						t.Fatal("submitted response after upload failure")
					}
					*out.(*upstreamResponse) = upstreamResponse{StatusCode: tc.status, Body: jsonBytes(tc.body)}
					return tc.err
				})
				request := imageRequest(map[string]any{"type": "input_image", "image_url": dataURL})
				request.Stream = method == "executor.execute_stream"
				request.StreamID = "test-downstream"
				_, err := service.Handle(method, jsonBytes(request))
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != tc.want || calls != 1 {
					t.Fatalf("error=%v calls=%d", err, calls)
				}
			})
		}
	}
}

func TestInvalidInlineImagesStopBeforeNetworking(t *testing.T) {
	for _, dataURL := range []string{"data:image/png;base64", "data:text/plain;base64,dGVzdA==", "data:image/png;base64,%%%", "data:image/png;base64,not-base64", "data:image/png;base64,", "data:,image"} {
		service := newHTTPTestService()
		service.SetHost(func(string, any, any) error { t.Error("invalid image reached network"); return nil })
		request := imageRequest(map[string]any{"type": "input_image", "image_url": dataURL})
		_, err := service.Handle("executor.execute", jsonBytes(request))
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Fatalf("expected invalid image status, got %v", err)
		}
	}
}

func TestAttachmentCacheCredentialAndEndpointIsolation(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	service := newHTTPTestService()
	uploads := 0
	service.SetHost(func(method string, payload any, out any) error {
		uploads++
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": fmt.Sprintf("file-%d", uploads)})}
		return nil
	})
	for _, tc := range []struct{ account, token, endpoint, want string }{
		{"account-a", "token-a", DefaultResponsesURL, "file-1"},
		{"account-a", "token-a", DefaultResponsesURL, "file-1"},
		{"account-b", "token-a", DefaultResponsesURL, "file-2"},
		{"account-a", "token-b", DefaultResponsesURL, "file-3"},
		{"account-a", "token-a", "https://custom.test/api/responses", "file-4"},
	} {
		service.cfg.ResponsesURL = tc.endpoint
		request := imageRequest(map[string]any{"type": "input_image", "image_url": dataURL})
		request.StorageJSON = jsonBytes(map[string]any{"account_id": tc.account, "access_token": tc.token})
		body, _, err := service.prepareRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if got := objectValue(lastUserContent(body)[0])["file_id"]; got != tc.want {
			t.Fatalf("file ID %v crossed an authentication boundary", got)
		}
	}
}

func TestAttachmentCacheCoalescesConcurrentUploads(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	service := newHTTPTestService()
	var uploads atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	service.SetHost(func(method string, payload any, out any) error {
		if uploads.Add(1) == 1 {
			close(started)
		}
		<-release
		*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-concurrent"})}
		return nil
	})
	request := imageRequest(map[string]any{"type": "input_image", "image_url": dataURL})
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			body, _, err := service.prepareRequest(request)
			if err != nil {
				t.Error(err)
				return
			}
			if objectValue(lastUserContent(body)[0])["file_id"] != "file-concurrent" {
				t.Error("concurrent caller got a different file ID")
			}
		})
	}
	<-started
	close(release)
	group.Wait()
	if uploads.Load() != 1 {
		t.Fatalf("duplicate concurrent uploads: %d", uploads.Load())
	}
}

func TestAttachmentCacheEvictsOldestAndDoesNotCacheFailures(t *testing.T) {
	var cache attachmentCache
	key := sha256.Sum256([]byte("first"))
	if _, err := cache.getOrUpload(key, func() (string, error) { return "", errors.New("failed") }); err == nil {
		t.Fatal("upload failure hidden")
	}
	if fileID, err := cache.getOrUpload(key, func() (string, error) { return "file-first", nil }); err != nil || fileID != "file-first" {
		t.Fatal("failed upload poisoned the next explicit request")
	}
	for i := range maxAttachmentCacheEntries {
		next := sha256.Sum256([]byte(fmt.Sprint(i)))
		_, _ = cache.getOrUpload(next, func() (string, error) { return "file-next", nil })
	}
	if len(cache.entries) != maxAttachmentCacheEntries || cache.entries[key] != nil || len(cache.pending) != 0 {
		t.Fatal("upload cache is unbounded or did not evict the oldest entry")
	}
}

func TestImageUploadPreservesOtherInputKinds(t *testing.T) {
	dataURL, _ := testImageDataURL(t)
	source := map[string]any{"input": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_image", "file_id": "file-existing", "detail": "high"},
			map[string]any{"type": "input_image", "image_url": "https://example.test/image.png", "detail": "auto"},
		}},
		map[string]any{"type": "function_call_output", "output": []any{map[string]any{"type": "input_image", "image_url": dataURL}}},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "input_image", "image_url": dataURL}}},
	}}
	before := string(jsonBytes(source))
	service := newHTTPTestService()
	service.SetHost(func(string, any, any) error {
		t.Error("unexpected upload or URL download")
		return errors.New("unexpected host call")
	})
	if err := service.uploadInputImages(ExecutorRequest{}, source, credential{}, defaultConfig()); err != nil {
		t.Fatal(err)
	}
	if string(jsonBytes(source)) != before {
		t.Fatal("modified existing ID, remote URL, or tool output")
	}
}

func TestAttachmentErrorsRedactCredentialsAndImageBytes(t *testing.T) {
	c := credential{AccessToken: strings.Repeat("secret-token", 100), AccountID: "private-account", Email: "private@example.test"}
	image := inlineImage{mediaType: "image/png", data: []byte("private-image-bytes")}
	encoded := base64.StdEncoding.EncodeToString(image.data)
	raw := jsonBytes(map[string]any{"message": strings.Join([]string{c.AccessToken, c.AccountID, c.Email, encoded, string(image.data)}, " ")})
	message := attachmentErrorMessage(raw, c, image)
	for _, secret := range []string{"secret-token", c.AccountID, c.Email, encoded, string(image.data)} {
		if strings.Contains(message, secret) {
			t.Fatal("upload error leaked private data")
		}
	}
}

func TestAttachmentURLRemainsOnConfiguredOrigin(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{DefaultResponsesURL, "https://bps.openai.com/basispoints/api/attachments"},
		{"https://custom.test:8443/custom/responses", "https://custom.test:8443/custom/attachments"},
		{"http://127.0.0.1:9876/v1/responses/", "http://127.0.0.1:9876/v1/attachments"},
	} {
		got, err := attachmentURL(tc.source)
		if err != nil || got != tc.want {
			t.Fatalf("endpoint=%s error=%v", got, err)
		}
	}
	for _, source := range []string{"/relative/responses", "https://user:pass@example.test/responses", "https://example.test/responses?secret=x"} {
		if _, err := attachmentURL(source); err == nil {
			t.Fatal("invalid attachment endpoint accepted")
		}
	}
}
