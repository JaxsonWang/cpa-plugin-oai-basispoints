package basispoints

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestUpstreamFailureClassificationAcrossResponsePaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		event  map[string]any
		status int
		code   string
	}{
		{"request", map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "code": "invalid_value", "message": "private-upstream-message"}}, 400, "invalid_value"},
		{"context", map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"code": "context_length_exceeded", "message": "private-upstream-message"}}}, 400, "context_length_exceeded"},
		{"signature", map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"code": "thinking_signature_invalid"}}}, 400, "thinking_signature_invalid"},
		{"explicit_validation", map[string]any{"type": "error", "status": 422, "error": map[string]any{"type": "invalid_request_error"}}, 422, "upstream_response_failed"},
		{"quota_status_wins", map[string]any{"type": "error", "status": 429, "error": map[string]any{"type": "invalid_request_error", "code": "invalid_value"}}, 429, "invalid_value"},
		{"request_code_with_server_status", map[string]any{"type": "error", "status": 502, "error": map[string]any{"code": "context_length_exceeded"}}, 502, "context_length_exceeded"},
		{"authentication", map[string]any{"type": "error", "error": map[string]any{"type": "authentication_error", "code": "invalid_api_key"}}, 401, "invalid_api_key"},
		{"rate_limit", map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"code": "rate_limit_exceeded"}}}, 429, "rate_limit_exceeded"},
		{"server", map[string]any{"type": "error", "status": 503, "error": map[string]any{"type": "server_error", "code": "server_error"}}, 503, "server_error"},
		{"unknown_private_fields", map[string]any{"type": "error", "status": 200, "error": map[string]any{"type": "private-error-type", "code": "private-error-code", "message": "private-upstream-message", "param": "private-prompt", "request_id": "private-token"}}, 502, "upstream_response_failed"},
		{"cancelled", map[string]any{"type": "response.cancelled", "response": map[string]any{"status": "cancelled"}}, 502, "upstream_response_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire strings.Builder
			writeSSE(&wire, stringValue(tc.event["type"]), tc.event)
			paths := map[string]func() error{
				"json_event": func() error {
					_, err := parseResponse(jsonBytes(tc.event), http.Header{"Content-Type": {"application/json"}})
					return err
				},
				"buffered_sse": func() error {
					_, err := parseResponse([]byte(wire.String()), http.Header{"Content-Type": {"text/event-stream"}})
					return err
				},
				"incremental": func() error {
					return newStreamDelivery("openai-response", func() {}, func([]byte) error { return nil }).consume(stringValue(tc.event["type"]), string(jsonBytes(tc.event)))
				},
			}
			if response := objectValue(tc.event["response"]); response != nil {
				paths["json"] = func() error {
					_, err := parseResponse(jsonBytes(response), http.Header{"Content-Type": {"application/json"}})
					return err
				}
			}
			for name, parse := range paths {
				t.Run(name, func(t *testing.T) {
					err := parse()
					var api *APIError
					if !errors.As(err, &api) || api.Status != tc.status || api.Kind != tc.code {
						t.Fatalf("error = %#v, want status=%d code=%s", err, tc.status, tc.code)
					}
					if strings.Contains(api.Message, "private-") || !strings.Contains(api.Message, "event="+stringValue(tc.event["type"])) {
						t.Fatalf("unsafe or missing failure diagnostic: %s", api.Message)
					}
					body, parseErr := rawObject([]byte(api.Error()))
					if parseErr != nil || objectValue(body["error"])["code"] != tc.code || objectValue(body["error"])["type"] != api.Type {
						t.Fatalf("classification lost in ABI error message: %s", api.Error())
					}
				})
			}
		})
	}
}

func TestCommittedStreamFailurePreservesStatus(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422, 429, 503} {
		var frame []byte
		delivery := newStreamDelivery("openai-response", func() {}, func(raw []byte) error { frame = append(frame, raw...); return nil })
		if err := delivery.fail(fail(status, "upstream_error", "safe diagnostic")); err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		if err := newSSEDecoder().feed(frame, func(_, data string) error { var err error; event, err = rawObject([]byte(data)); return err }); err != nil {
			t.Fatal(err)
		}
		if got := string(jsonBytes(event["status"])); got != string(jsonBytes(status)) {
			t.Fatalf("status missing from committed failure: %s", frame)
		}
	}
}
