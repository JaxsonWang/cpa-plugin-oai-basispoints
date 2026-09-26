package basispoints

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTextFormatRejectsUnsupportedAndInvalidRequestsBeforeHost(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, sourceFormat := range []string{"openai-response", "codex"} {
			for _, tc := range []struct {
				name, kind string
				text       any
			}{
				{"json_schema", "unsupported_text_format", map[string]any{"format": map[string]any{"type": "json_schema", "name": "private-schema", "strict": true, "schema": map[string]any{"private-field": "private-value"}}}},
				{"json_object", "unsupported_text_format", map[string]any{"format": map[string]any{"type": "json_object"}}},
				{"text_string", "invalid_text_config", "private-value"},
				{"format_string", "invalid_text_format", map[string]any{"format": "private-value"}},
				{"empty_format", "invalid_text_format", map[string]any{"format": map[string]any{}}},
				{"unknown_type", "invalid_text_format", map[string]any{"format": map[string]any{"type": "private-type"}}},
				{"plain_extra", "invalid_text_format", map[string]any{"format": map[string]any{"type": "text", "schema": "private-value"}}},
			} {
				t.Run(sourceFormat+"/"+tc.name+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
					svc := NewService()
					calls := 0
					svc.SetHost(func(string, any, any) error { calls++; return nil })
					source := map[string]any{"model": DefaultModelID, "input": "hello", "text": tc.text}
					request := ExecutorRequest{Model: DefaultModelID, Format: sourceFormat, SourceFormat: sourceFormat, Stream: stream, Payload: jsonBytes(source), OriginalRequest: jsonBytes(map[string]any{"model": "ignored-original"}), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}, StreamID: "fixture-stream"}
					if sourceFormat == "openai-response" {
						request.OriginalRequest = nil
					}
					method := "executor.execute"
					if stream {
						method = "executor.execute_stream"
					}
					_, err := svc.Handle(method, jsonBytes(request))
					var apiError *APIError
					if !errors.As(err, &apiError) || apiError.Status != 400 || apiError.Kind != tc.kind || calls != 0 {
						t.Fatalf("error=%v host_calls=%d", err, calls)
					}
					if strings.Contains(err.Error(), "private-") {
						t.Fatal("error leaked request schema or values")
					}
				})
			}
		}
	}
}

func TestTextFormatPreservesPlainAndExistingClientDefaults(t *testing.T) {
	baseline := map[string]any{"model": DefaultModelID, "input": "hello", "prompt_cache_key": "same-conversation"}
	want, err := prepareResponsesBody(baseline, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []any{nil, map[string]any{}, map[string]any{"format": nil}, map[string]any{"format": map[string]any{"type": "text"}}, map[string]any{"verbosity": "low"}, map[string]any{"verbosity": "low", "format": map[string]any{"type": "text"}}} {
		source := cloneObject(baseline)
		source["text"] = text
		before := string(jsonBytes(source))
		got, err := prepareResponsesBody(source, defaultConfig())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("plain/default request changed: text=%v err=%v", text, err)
		}
		if string(jsonBytes(source)) != before {
			t.Fatal("mutated caller request")
		}
	}
}
