package basispoints

import (
	"testing"
)

func TestUnsupportedRequestContracts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, alt, field string
			value            any
			kind             string
		}{
			{name: "compact", alt: "responses/compact", kind: "unsupported_compaction"},
			{name: "previous_response_id", field: "previous_response_id", value: "resp_previous", kind: "unsupported_continuation"},
			{name: "priority", field: "service_tier", value: "priority", kind: "unsupported_service_tier"},
			{name: "fast", field: "service_tier", value: "fast", kind: "unsupported_service_tier"},
		} {
			t.Run(tc.name+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				s := NewService()
				calls := 0
				s.SetHost(func(method string, payload any, out any) error { calls++; return nil })
				source := map[string]any{"model": DefaultModelID, "input": "Hello", "stream": stream}
				if tc.field != "" {
					source[tc.field] = tc.value
				}
				req := ExecutorRequest{Model: DefaultModelID, Alt: tc.alt, Stream: stream, Payload: jsonBytes(source), StorageJSON: jsonBytes(map[string]any{"access_token": "fixture-token", "account_id": "fixture-account"})}
				_, _, err := s.prepareRequest(req)
				api, ok := err.(*APIError)
				if !ok || api.Status != 400 || api.Kind != tc.kind {
					t.Fatalf("want 400 %s, got %v", tc.kind, err)
				}
				if calls != 0 {
					t.Fatalf("unsupported request reached host %d times", calls)
				}
			})
		}
	}
}

func TestStandardServiceTierIsOmitted(t *testing.T) {
	for _, tier := range []any{nil, "auto", "default"} {
		source := map[string]any{"model": DefaultModelID, "input": "Hello", "service_tier": tier}
		body, err := prepareResponsesBody(source, defaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := body["service_tier"]; exists {
			t.Fatalf("ordinary tier %v must not be sent to Basis Points", tier)
		}
	}
}

func TestStandardTierAcrossExecutorSources(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, original := range []bool{false, true} {
			for _, tier := range []any{nil, "auto", "default", "priority", "flex", "", map[string]any{"invalid": true}} {
				source := map[string]any{"model": DefaultModelID, "input": "Hello", "service_tier": tier, "previous_response_id": nil}
				raw := jsonBytes(source)
				req := ExecutorRequest{Model: DefaultModelID, Payload: raw, Stream: stream, StorageJSON: jsonBytes(map[string]any{"access_token": "fixture", "account_id": "fixture"})}
				if original {
					req.OriginalRequest = raw
					req.Payload = jsonBytes(map[string]any{"model": "ignored"})
				}
				body, _, err := NewService().prepareRequest(req)
				valid := tier == nil || tier == "auto" || tier == "default"
				if valid {
					if err != nil {
						t.Fatal(err)
					}
					if _, exists := body["service_tier"]; exists {
						t.Fatal("unsupported upstream field transmitted")
					}
					if body["stream"] != stream {
						t.Fatal("stream mode changed")
					}
				} else {
					api, ok := err.(*APIError)
					if !ok || api.Status != 400 || api.Kind != "unsupported_service_tier" {
						t.Fatalf("tier=%v error=%v", tier, err)
					}
				}
			}
		}
	}
}
