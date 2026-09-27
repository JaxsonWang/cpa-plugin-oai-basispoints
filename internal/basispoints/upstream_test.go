package basispoints

import (
	"errors"
	"strings"
	"testing"
)

func TestRejectedStreamPreservesBodyStatusAndCloses(t *testing.T) {
	cases := []struct {
		name   string
		chunks []streamChunk
		want   string
		status int
	}{
		{"validation", []streamChunk{{Payload: []byte(`{"error":{"message":"Invalid `)}, {Payload: []byte(`request body"}}`), Done: true}}, "Invalid request body", 422},
		{"empty", []streamChunk{{Done: true}}, "upstream request failed", 422},
		{"interrupted", []streamChunk{{Error: "test read interruption", Done: true}}, "test read interruption", 422},
		{"unauthorized", []streamChunk{{Payload: []byte(`{"message":"expired token"}`), Done: true}}, "expired token", 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := newHTTPTestService()
			opens, reads, closes := 0, 0, 0
			service.SetHost(func(method string, payload any, out any) error {
				switch method {
				case "host.http.do_stream":
					opens++
					*out.(*upstreamStream) = upstreamStream{StatusCode: tc.status, StreamID: "upstream-test"}
				case "host.http.stream_read":
					if reads >= len(tc.chunks) {
						t.Fatal("read past end")
					}
					*out.(*streamChunk) = tc.chunks[reads]
					reads++
				case "host.http.stream_close":
					closes++
				default:
					t.Fatalf("unexpected host method %s", method)
				}
				return nil
			})
			_, err := service.upstreamStream(ExecutorRequest{}, map[string]any{"reasoning_effort": "ultra"}, credential{})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.status || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v", err)
			}
			if opens != 1 || reads != len(tc.chunks) || closes != 1 {
				t.Fatalf("opens=%d reads=%d closes=%d", opens, reads, closes)
			}
			if tc.name != "interrupted" && !strings.Contains(err.Error(), "reasoning_effort=ultra") {
				t.Fatal("missing request diagnostic")
			}
		})
	}
}

func TestUpstreamDiagnosticRedactsCredentialsAndOmitsImageData(t *testing.T) {
	c := credential{AccessToken: "test-secret-token", AccountID: "test-secret-account", Email: "private@example.test"}
	body := map[string]any{"reasoning_effort": "xhigh", "input": []any{map[string]any{"content": []any{map[string]any{"type": "input_image", "image_url": "private-image-data"}}}}}
	err := upstreamRequestError(422, jsonBytes(map[string]any{"message": c.AccessToken + " " + c.AccountID + " " + c.Email}), body, c)
	for _, secret := range []string{c.AccessToken, c.AccountID, c.Email, "private-image-data"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("diagnostic exposed private data")
		}
	}
	if !strings.Contains(err.Error(), "input_images=1") || !strings.Contains(err.Error(), "reasoning_effort=xhigh") {
		t.Fatal("diagnostic missing safe request summary")
	}
}

func TestValidationErrorOmitsRejectedInput(t *testing.T) {
	raw := []byte(`{"detail":[{"loc":["body","reasoning_effort"],"msg":"unsupported level","type":"enum","input":"private-prompt","ctx":{"input":"private-context"}}]}`)
	message := errorMessage(raw)
	if !strings.Contains(message, "reasoning_effort") || !strings.Contains(message, "unsupported level") {
		t.Fatal("validation details missing")
	}
	if strings.Contains(message, "private-") {
		t.Fatal("rejected input exposed")
	}
}

func TestSuccessfulStreamIsNotReadOrClosedEarly(t *testing.T) {
	service := newHTTPTestService()
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.http.do_stream" {
			t.Fatalf("success stream touched early: %s", method)
		}
		*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "success"}
		return nil
	})
	stream, err := service.upstreamStream(ExecutorRequest{}, map[string]any{}, credential{})
	if err != nil || stream.StreamID != "success" {
		t.Fatalf("stream=%+v error=%v", stream, err)
	}
}

func TestLongCredentialIsRedactedBeforeErrorTruncation(t *testing.T) {
	c := credential{AccessToken: strings.Repeat("private-secret", 100)}
	err := upstreamRequestError(422, []byte("rejected "+c.AccessToken), map[string]any{}, c)
	if strings.Contains(err.Error(), "private-secret") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatal("truncated credential exposed")
	}
}

func TestUpstreamDiagnosticReportsOnlySafeServiceTier(t *testing.T) {
	for _, tc := range []struct {
		tier any
		want string
	}{
		{"priority", "priority"}, {"default", "default"}, {"private-request-data", "invalid"},
		{map[string]any{"private": "request-data"}, "invalid"},
	} {
		err := upstreamRequestError(422, []byte(`{"message":"Invalid request body"}`), map[string]any{"service_tier": tc.tier}, credential{})
		if !strings.Contains(err.Error(), "service_tier="+tc.want) || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe or missing tier: %v", err)
		}
	}
	err := upstreamRequestError(422, nil, map[string]any{}, credential{})
	if !strings.Contains(err.Error(), "service_tier=unspecified") {
		t.Fatal("missing absent tier diagnostic")
	}
}
