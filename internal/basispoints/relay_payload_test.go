package basispoints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func rawRelayNative(id, name string, code any, references any) map[string]any {
	return map[string]any{
		"type": "function_call", "name": transportName, "call_id": id,
		"arguments": string(jsonBytes(map[string]any{
			"summary":          "Run client tool " + name,
			"extended_summary": "Relay one client tool payload without an inner wrapper",
			"destructive":      false, "references": references, "code": code,
		})),
	}
}

func TestRelayPayloadAvoidsNestedJSON(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: sample.js\n+await page.locator('[data-id=\"1\"]').click();\n*** End Patch\n"
	for _, tc := range []struct {
		name    string
		kind    string
		payload string
	}{
		{"quoted_patch", "custom", patch},
		{"many_quotes", "custom", strings.Repeat(patch, 100)},
		{"whitespace", "custom", "  中文\r\n\t\"quoted\"\n  "},
		{"empty", "custom", ""},
		{"literal_escapes", "custom", `C:\new\test \n \t \u4e2d "quotes"`},
		{"json_like_custom", "custom", `{"tool":"do_not_execute","args":{}}`},
		{"function", "function", `{"cmd":"printf \"hello\"","id":9007199254740993}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := namespaceTestSource(tc.kind, "invoke", "tools")
			native := rawRelayNative(t.Name(), "tools.invoke", tc.payload, []any{"tools.invoke"})
			call, err := extractNativeClientToolCall(native, clientToolSpecs(source))
			if err != nil {
				t.Fatal(err)
			}
			if call["namespace"] != "tools" || call["name"] != "invoke" {
				t.Fatal("tool identity changed")
			}
			if tc.kind == "custom" {
				if call["input"] != tc.payload {
					t.Fatal("raw input bytes changed")
				}
			} else {
				parsed, reason := parseRelayObject(call["arguments"])
				if reason != "" || parsed["id"] != json.Number("9007199254740993") {
					t.Fatal("function payload changed")
				}
			}
			replay := fallbackTransportCall(call)
			outer := parseArguments(replay["arguments"])
			if !reflect.DeepEqual(outer["references"], []any{"tools.invoke"}) {
				t.Fatal("history lost tool routing")
			}
			restored, err := extractNativeClientToolCall(replay, clientToolSpecs(source))
			if err != nil {
				t.Fatal(err)
			}
			field := "arguments"
			if tc.kind == "custom" {
				field = "input"
			}
			if restored[field] != call[field] {
				t.Fatal("history changed payload")
			}
		})
	}
}

func TestRelayRoutingRejectsMalformedCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs any
		code any
	}{
		{"missing_reference", nil, `{}`},
		{"empty_references", []any{}, `{}`},
		{"multiple_references", []any{"tools.invoke", "tools.invoke"}, `{}`},
		{"reference_not_array", "tools.invoke", `{}`},
		{"reference_not_string", []any{1}, `{}`},
		{"empty_name", []any{""}, `{}`},
		{"native_name", []any{transportName}, `{}`},
		{"native_alias", []any{transportAlias}, `{}`},
		{"unknown_name", []any{"not_in_catalog"}, `{}`},
		{"unqualified_name", []any{"invoke"}, `{}`},
		{"padded_name", []any{" tools.invoke "}, `{}`},
		{"code_not_string", []any{"tools.invoke"}, map[string]any{}},
		{"invalid_json", []any{"tools.invoke"}, `{"cmd":"PRIVATE "quote""}`},
		{"trailing_json", []any{"tools.invoke"}, `{} {}`},
		{"null_json", []any{"tools.invoke"}, `null`},
		{"array_json", []any{"tools.invoke"}, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := namespaceTestSource("function", "invoke", "tools")
			native := rawRelayNative(t.Name(), "tools.invoke", tc.code, tc.refs)
			_, err := extractNativeClientToolCall(native, clientToolSpecs(source))
			api, ok := err.(*APIError)
			if !ok || api.Status != 422 || api.Kind != "invalid_tool_call" {
				t.Fatalf("invalid relay accepted: %v", err)
			}
			if strings.Contains(api.Message, "PRIVATE") {
				t.Fatal("diagnostic leaked tool content")
			}
		})
	}
}

func TestRelayValidationIsAtomic(t *testing.T) {
	source := namespaceTestSource("custom", "apply_patch", "")
	good := rawRelayNative(t.Name()+"-good", "apply_patch", `unchanged "raw" input`, []any{"apply_patch"})
	bad := rawRelayNative(t.Name()+"-bad", "apply_patch", "private", []any{})
	original := map[string]any{"status": "completed", "output": []any{good, bad}}
	before := string(jsonBytes(original))
	body, response, changed, err := transformResponseBody(jsonBytes(original), source)
	if err == nil || body != nil || response != nil || changed {
		t.Fatal("failed response partially delivered")
	}
	for _, call := range []map[string]any{good, bad} {
		if rememberedNativeCall(stringValue(call["call_id"])) != nil {
			t.Fatal("failed response partially cached")
		}
	}
	if string(jsonBytes(original)) != before {
		t.Fatal("original payload mutated")
	}
}

func TestRelayPreservesPreviousNativeHistory(t *testing.T) {
	for _, outerName := range []string{transportName, transportAlias} {
		t.Run(outerName, func(t *testing.T) {
			legacy := map[string]any{
				"type": "function_call", "name": outerName, "call_id": t.Name(),
				"arguments": string(jsonBytes(map[string]any{
					"code": string(jsonBytes(map[string]any{"tool": "apply_patch", "args": `old "quoted" patch`})),
				})),
			}
			source := namespaceTestSource("custom", "apply_patch", "")
			result := map[string]any{"type": "custom_tool_call_output", "call_id": t.Name(), "output": "already executed"}
			replay := translateInputItems([]any{legacy, result})
			if len(replay) != 2 || !reflect.DeepEqual(replay[0], legacy) {
				t.Fatal("old native history rewritten")
			}
			output := objectValue(replay[1])
			if output["type"] != "function_call_output" || output["call_id"] != t.Name() || output["output"] != "already executed" {
				t.Fatal("history result lost")
			}
			catalog := clientToolProtocolInstructions(source)
			if !strings.Contains(catalog, "Historical calls") || !strings.Contains(catalog, "do not copy that format into new calls") {
				t.Fatal("new turn can inherit old relay instructions")
			}
		})
	}
}
