package basispoints

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestImageUploadsUseContentTypeAndSupportedFilename(t *testing.T) {
	_, pngData := testImageDataURL(t)
	var jpegData, gifData bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, 3, 2))
	if err := jpeg.Encode(&jpegData, canvas, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, canvas, nil); err != nil {
		t.Fatal(err)
	}
	// WebP 用例只核对格式签名和上传字节，不模拟远端图像解码成功。
	webpData := []byte("RIFF\x16\x00\x00\x00WEBPVP8 \x0a\x00\x00\x00\x00\x00\x00\x9d\x01\x2a\x01\x00\x01\x00")
	for _, tc := range []struct {
		name, declared, contentType, filename string
		data                                  []byte
	}{
		{"png", "image/png", "image/png", "image.png", pngData},
		{"jpeg", "image/jpeg", "image/jpeg", "image.jpeg", jpegData.Bytes()},
		{"jpeg_alias", "image/jpg", "image/jpeg", "image.jpeg", jpegData.Bytes()},
		{"gif", "image/gif", "image/gif", "image.gif", gifData.Bytes()},
		{"webp", "image/webp", "image/webp", "image.webp", webpData},
		{"unknown_declared_type", "image/unknown", "image/png", "image.png", pngData},
		{"incorrect_declared_type", "image/jpeg", "image/png", "image.png", pngData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newHTTPTestService()
			uploads := 0
			service.SetHost(func(method string, payload any, out any) error {
				uploads++
				wire := payload.(map[string]any)
				if method != "host.http.do" || !strings.HasSuffix(wire["url"].(string), "/attachments") {
					t.Fatal("unexpected host call")
				}
				_, params, err := mime.ParseMediaType(wire["headers"].(http.Header).Get("Content-Type"))
				if err != nil {
					t.Fatal(err)
				}
				part, err := multipart.NewReader(bytes.NewReader(wire["body"].([]byte)), params["boundary"]).NextPart()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(part)
				if err != nil || !bytes.Equal(data, tc.data) {
					t.Fatal("upload changed image bytes")
				}
				if part.FileName() != tc.filename || part.Header.Get("Content-Type") != tc.contentType {
					t.Errorf("upload filename=%q type=%q; want filename=%q type=%q", part.FileName(), part.Header.Get("Content-Type"), tc.filename, tc.contentType)
				}
				*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200, Body: jsonBytes(map[string]any{"openai_file_id": "file-format"})}
				return nil
			})
			request := imageRequest(map[string]any{"type": "input_image", "image_url": "data:" + tc.declared + ";base64," + base64.StdEncoding.EncodeToString(tc.data)})
			body, _, err := service.prepareRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			if uploads != 1 || !reflect.DeepEqual(objectValue(lastUserContent(body)[0]), map[string]any{"type": "input_image", "file_id": "file-format"}) {
				t.Fatal("image did not upload exactly once with a valid file reference")
			}
		})
	}
}

func TestUnrecognizedImageDataReportsPositionBeforeNetworking(t *testing.T) {
	for _, tc := range []struct{ name, mediaType, data string }{
		{"text_as_png", "image/png", "private-image-content"},
		{"unsupported_svg", "image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg"></svg>`},
		{"unknown", "image/unknown", "private-unknown-content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newHTTPTestService()
			service.SetHost(func(string, any, any) error {
				t.Fatal("unrecognized image reached network")
				return nil
			})
			request := imageRequest(
				map[string]any{"type": "input_text", "text": "private-prompt"},
				map[string]any{"type": "input_image", "image_url": "data:" + tc.mediaType + ";base64," + base64.StdEncoding.EncodeToString([]byte(tc.data))},
			)
			_, _, err := service.prepareRequest(request)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Kind != "invalid_image" || !strings.Contains(err.Error(), "input[1].content[1]") || !strings.Contains(err.Error(), "PNG, JPEG, GIF, or WebP") {
				t.Fatalf("missing safe image format diagnostic: %v", err)
			}
			if strings.Contains(err.Error(), tc.data) || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "base64") {
				t.Fatal("image diagnostic exposed input content")
			}
		})
	}
}

func TestTextImagePathsDoNotCreateImageInputs(t *testing.T) {
	service := newHTTPTestService()
	service.SetHost(func(string, any, any) error {
		t.Fatal("text path caused an attachment request")
		return nil
	})
	text := `Screenshot: C:\Users\example\.codex\visualizations\result.png; ![plot](/tmp/plot.png)`
	request := imageRequest(map[string]any{"type": "input_text", "text": text})
	body, _, err := service.prepareRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lastUserContent(body), []any{map[string]any{"type": "input_text", "text": text}}) {
		t.Fatal("text image paths were converted or changed")
	}
	err = upstreamRequestError(400, []byte(`{"message":"fixture rejection"}`), body, credential{})
	if !strings.Contains(err.Error(), "input_images=0") {
		t.Fatal("text image paths were counted as image inputs")
	}
}

func TestCodexImageCountUsesTranslatedPayload(t *testing.T) {
	service := newHTTPTestService()
	service.SetHost(func(string, any, any) error {
		t.Fatal("existing image IDs caused an attachment request")
		return nil
	})
	var images []any
	for i := 0; i < 9; i++ {
		images = append(images, map[string]any{"type": "input_image", "file_id": fmt.Sprintf("file-private-%d", i), "detail": "auto"})
	}
	request := imageRequest(images...)
	request.OriginalRequest = imageRequest(map[string]any{"type": "input_text", "text": `C:\private\visualizations\result.png`}).Payload
	// 构造宿主转换前后不同的输入，不据此断言现场的九张图片来源已经确认。
	for _, tc := range []struct{ sourceFormat, count string }{{"codex", "9"}, {"openai", "0"}} {
		t.Run(tc.sourceFormat, func(t *testing.T) {
			request.SourceFormat = tc.sourceFormat
			body, _, err := service.prepareRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			err = upstreamRequestError(400, []byte(`{"message":"fixture rejection"}`), body, credential{})
			if !strings.Contains(err.Error(), "input_images="+tc.count+";") || strings.Contains(err.Error(), "private") {
				t.Fatalf("incorrect or unsafe image summary: %v", err)
			}
		})
	}
}
