package basispoints

import (
	"errors"
	"strings"
	"testing"
)

func TestRelayRejectsMalformedJSONWithoutRepair(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		outer   bool
	}{
		{"raw_newline", "{\"cmd\":\"PRIVATE\nvalue\"}", false},
		{"raw_tab", "{\"cmd\":\"PRIVATE\tvalue\"}", false},
		{"markdown_fence", "```json\n{\"cmd\":\"PRIVATE\"}\n```", false},
		{"fence_trailing_content", "```json\n{\"cmd\":\"PRIVATE\"}\n```\n{\"second\":true}", false},
		{"outer_fence_trailing_content", "", true},
		{"trailing_comma", `{"cmd":"PRIVATE",}`, false},
		{"double_encoded", `"{\"cmd\":\"PRIVATE\"}"`, false},
		{"missing_array_value", `{"targets":[,]}`, false},
		{"missing_object_member", `{,}`, false},
		{"missing_quote", `{"cmd":"PRIVATE "quote""}`, false},
		{"truncated_object", `{"cmd":`, false},
		{"array", `[]`, false},
		{"null", `null`, false},
		{"two_objects", `{} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := namespaceTestSource("function", "invoke", "tools")
			good := rawRelayNative(t.Name()+"-good", "tools.invoke", `{"cmd":"valid"}`, []any{"tools.invoke"})
			bad := rawRelayNative(t.Name()+"-bad", "tools.invoke", tc.payload, []any{"tools.invoke"})
			if tc.outer {
				bad = rawRelayNative(t.Name()+"-bad", "tools.invoke", `{"cmd":"PRIVATE"}`, []any{"tools.invoke"})
				bad["arguments"] = "```json\n" + bad["arguments"].(string) + "\n```\n{\"second\":true}"
			}
			original := map[string]any{"status": "completed", "output": []any{good, bad}}
			before := string(jsonBytes(original))
			body, response, changed, err := transformResponseBody(jsonBytes(original), source)
			var api *APIError
			if !errors.As(err, &api) || api.Status != 422 || api.Kind != "invalid_tool_call" || body != nil || response != nil || changed {
				t.Fatalf("malformed batch was not rejected atomically: changed=%t err=%v", changed, err)
			}
			if strings.Contains(api.Message, "PRIVATE") {
				t.Fatal("diagnostic leaked tool input")
			}
			for _, native := range []map[string]any{good, bad} {
				if rememberedNativeCall(stringValue(native["call_id"])) != nil {
					t.Fatal("failed batch was partially cached")
				}
			}
			if string(jsonBytes(original)) != before {
				t.Fatal("original response was mutated")
			}
		})
	}
}

func TestRelayCustomPayloadDoesNotRequireJSON(t *testing.T) {
	for _, payload := range []string{"```json\n{,}\n```\ntrailing text", `{"targets":[,]}`, "  PRIVATE\n\tvalue\r\n"} {
		t.Run(payload, func(t *testing.T) {
			source := namespaceTestSource("custom", "invoke", "tools")
			native := rawRelayNative(t.Name(), "tools.invoke", payload, []any{"tools.invoke"})
			call, err := extractNativeClientToolCall(native, clientToolSpecs(source))
			if err != nil || call["type"] != "custom_tool_call" || call["input"] != payload {
				t.Fatalf("custom raw input was rejected or changed: %v", err)
			}
		})
	}
}
