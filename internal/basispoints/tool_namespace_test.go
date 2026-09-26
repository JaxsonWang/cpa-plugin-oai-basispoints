package basispoints

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNamespacedToolCallPreservesNamespace(t *testing.T) {
	source := map[string]any{"tools": []any{map[string]any{
		"type": "namespace", "name": "mcp__node_repl",
		"tools": []any{map[string]any{
			"type": "function", "name": "js",
			"parameters": map[string]any{
				"type": "object", "required": []any{"code"},
				"properties": map[string]any{"code": map[string]any{"type": "string"}},
			},
		}},
	}}}
	native := namespaceTestNative("namespace_repro", "mcp__node_repl.js", map[string]any{"code": "nodeRepl.write('ok')"})
	call, callErr := extractNativeClientToolCall(native, clientToolSpecs(source))
	if callErr != nil {
		t.Fatal("namespaced tool was not decoded")
	}
	if call["name"] != "js" || call["namespace"] != "mcp__node_repl" {
		t.Fatalf("call lost its qualified identity: %#v", call)
	}
	if got := call["arguments"]; got != `{"code":"nodeRepl.write('ok')"}` {
		t.Fatalf("arguments = %v", got)
	}
	if _, _, _, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source); err != nil {
		t.Fatal(err)
	}
	if replay := translateInputItems([]any{call}, clientToolSpecs(source)); !reflect.DeepEqual(replay[0], native) {
		t.Fatalf("cached replay = %#v, want %#v", replay[0], native)
	}
}

func namespaceTestSource(toolType, name, namespace string) map[string]any {
	tool := map[string]any{"type": toolType, "name": name}
	if toolType == "function" {
		tool["parameters"] = map[string]any{"type": "object"}
	}
	if namespace != "" {
		tool = map[string]any{"type": "namespace", "name": namespace, "tools": []any{tool}}
	}
	return map[string]any{"tools": []any{tool}}
}

func relayTestPayload(args any) string {
	if text, ok := args.(string); ok {
		return text
	}
	return string(jsonBytes(args))
}

func namespaceTestNative(id, key string, args any) map[string]any {
	return map[string]any{
		"type": "function_call", "name": transportName,
		"id": "fc_" + id, "call_id": "call_" + id, "status": "completed",
		"summary": "Relay a client tool", "references": []any{"client"},
		"arguments": string(jsonBytes(map[string]any{
			"code": relayTestPayload(args), "references": []any{key},
		})),
	}
}

func TestClientToolIdentityAndReplay(t *testing.T) {
	for _, tc := range []struct{ kind, name, namespace string }{
		{"function", "js", "mcp__node_repl"},
		{"custom", "apply_patch", "functions"},
		{"function", "exec_command", ""},
		{"custom", "apply_patch", ""},
		{"function", "mcp__node_repl.js", ""},
		{"function", "mcp__node_repl__js", ""},
	} {
		t.Run(tc.kind+"/"+tc.namespace+"/"+tc.name, func(t *testing.T) {
			source := namespaceTestSource(tc.kind, tc.name, tc.namespace)
			key := tc.name
			if tc.namespace != "" {
				key = tc.namespace + "." + tc.name
			}
			var args any = map[string]any{"code": "  原始输入\n"}
			field, itemType, outputType := "arguments", "function_call", "function_call_output"
			if tc.kind == "custom" {
				args = "  原始输入\r\n\t保留空白\n"
				field, itemType, outputType = "input", "custom_tool_call", "custom_tool_call_output"
			}
			native := namespaceTestNative(t.Name(), key, args)
			_, response, changed, err := transformResponseBody(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}), source)
			if err != nil || !changed {
				t.Fatalf("transform: changed=%t err=%v", changed, err)
			}
			call := objectValue(response["output"].([]any)[0])
			if call["type"] != itemType || call["name"] != tc.name || stringValue(call["namespace"]) != tc.namespace || call["call_id"] != native["call_id"] || call["id"] != native["id"] {
				t.Fatalf("client identity changed: %#v", call)
			}
			if tc.namespace == "" {
				if _, present := call["namespace"]; present {
					t.Fatal("added namespace to a flat tool")
				}
			}
			want := args
			if field == "arguments" {
				want = string(jsonBytes(args))
			}
			if call[field] != want {
				t.Fatalf("%s = %q, want %q", field, call[field], want)
			}
			if tc.kind == "custom" {
				if _, present := call["arguments"]; present {
					t.Fatal("custom input was changed into function arguments")
				}
			}
			for _, output := range []any{"", "  text\n", []any{}, []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,dGVzdA=="}}} {
				for _, cached := range []bool{true, false} {
					historyCall := cloneObject(call)
					if !cached {
						historyCall["call_id"] = stringValue(call["call_id"]) + "_cache_miss"
					}
					result := map[string]any{"type": outputType, "call_id": historyCall["call_id"], "name": tc.name, "namespace": tc.namespace, "output": output}
					replay := translateInputItems([]any{historyCall, result}, clientToolSpecs(source))
					if len(replay) != 2 {
						t.Fatalf("replay length = %d", len(replay))
					}
					replayedCall, replayedOutput := objectValue(replay[0]), objectValue(replay[1])
					if cached && !reflect.DeepEqual(replayedCall, native) {
						t.Fatalf("native replay changed: %#v", replayedCall)
					}
					envelope, _ := transportEnvelope(replayedCall)
					if envelope["tool"] != key || envelope["args"] != relayTestPayload(args) {
						t.Fatalf("replay envelope = %#v", envelope)
					}
					if replayedOutput["type"] != "function_call_output" || replayedOutput["call_id"] != replayedCall["call_id"] || !reflect.DeepEqual(replayedOutput["output"], output) {
						t.Fatalf("tool result changed: %#v", replayedOutput)
					}
					for _, key := range []string{"name", "namespace"} {
						if _, present := replayedOutput[key]; present {
							t.Fatalf("client %s leaked into upstream output", key)
						}
					}
				}
			}
		})
	}
}

func TestClientToolMultipleCallsKeepNamespaceIsolation(t *testing.T) {
	source := namespaceTestSource("function", "js", "alpha")
	source["tools"] = append(source["tools"].([]any), namespaceTestSource("function", "js", "beta")["tools"].([]any)...)
	first := namespaceTestNative(t.Name()+"_first", "alpha.js", map[string]any{})
	second := namespaceTestNative(t.Name()+"_second", "beta.js", map[string]any{})
	message := messageItem("assistant", "Running two distinct tools.")
	_, response, changed, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{message, first, second}}), source)
	if err != nil || !changed {
		t.Fatalf("transform changed=%t err=%v", changed, err)
	}
	output := response["output"].([]any)
	if len(output) != 3 || !reflect.DeepEqual(output[0], message) || objectValue(output[1])["namespace"] != "alpha" || objectValue(output[2])["namespace"] != "beta" {
		t.Fatalf("output order or namespaces changed: %#v", output)
	}
	source["parallel_tool_calls"] = false
	if _, _, _, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{first, second}}), source); err == nil {
		t.Fatal("parallel_tool_calls=false was ignored")
	}
}

func TestClientToolArgumentsPreserveLargeIntegers(t *testing.T) {
	source := namespaceTestSource("function", "js", "mcp__node_repl")
	args := map[string]any{"id": json.Number("9007199254740993"), "nested": []any{json.Number("18446744073709551615")}}
	native := namespaceTestNative(t.Name(), "mcp__node_repl.js", args)
	_, response, _, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source)
	if err != nil {
		t.Fatal(err)
	}
	call := objectValue(response["output"].([]any)[0])
	if call["arguments"] != string(jsonBytes(args)) {
		t.Fatalf("integer precision lost: %s", call["arguments"])
	}
	call["call_id"] = "call_uncached_" + t.Name()
	replay := translateInputItems([]any{call}, clientToolSpecs(source))
	if envelope, _ := transportEnvelope(objectValue(replay[0])); envelope["args"] != relayTestPayload(args) {
		t.Fatalf("replay lost integer precision: %#v", envelope)
	}
}

func TestClientToolRejectsInvalidCallsWithoutLeakingNativeTools(t *testing.T) {
	for _, name := range []string{"unknown-tool", "unqualified-name", "native-tool", "invalid-arguments", "missing-required", "extra-json", "missing-call-id", "duplicate-call-id", "tools-disabled", "custom-object"} {
		t.Run(name, func(t *testing.T) {
			source := namespaceTestSource("function", "js", "mcp__node_repl")
			native := namespaceTestNative(t.Name(), "mcp__node_repl.js", map[string]any{})
			output := []any{native}
			switch name {
			case "unknown-tool":
				native = namespaceTestNative(t.Name(), "unknown.js", map[string]any{})
			case "unqualified-name":
				native = namespaceTestNative(t.Name(), "js", map[string]any{})
			case "native-tool":
				native["name"] = "read_sheets_metadata"
			case "invalid-arguments":
				native = namespaceTestNative(t.Name(), "mcp__node_repl.js", "private-invalid-args")
			case "missing-required":
				spec := clientToolSpecs(source)["mcp__node_repl.js"]
				spec.Spec["parameters"] = map[string]any{"type": "object", "required": []any{"code"}}
			case "extra-json":
				outer := parseArguments(native["arguments"])
				outer["code"] = outer["code"].(string) + " {}"
				native["arguments"] = string(jsonBytes(outer))
			case "missing-call-id":
				delete(native, "call_id")
			case "duplicate-call-id":
				output = append(output, native)
			case "tools-disabled":
				source["tool_choice"] = "none"
			case "custom-object":
				source = namespaceTestSource("custom", "js", "mcp__node_repl")
				outer := parseArguments(native["arguments"])
				outer["code"] = map[string]any{"private": "not raw text"}
				native["arguments"] = string(jsonBytes(outer))
			}
			output[0] = native
			body, response, changed, err := transformResponseBody(jsonBytes(map[string]any{"output": output}), source)
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.Status != 422 || apiError.Kind != "invalid_tool_call" || body != nil || response != nil || changed {
				t.Fatalf("invalid call leaked: body=%s changed=%t err=%v", body, changed, err)
			}
			if strings.Contains(err.Error(), "private-invalid-args") {
				t.Fatal("error exposed tool input")
			}
			if rememberedNativeCall(stringValue(native["call_id"])) != nil {
				t.Fatal("failed response was partially cached")
			}
		})
	}
}

func TestToolCatalogPreservesNestedSchemaAndCustomFormat(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []any{"target"}, "properties": map[string]any{
		"target": map[string]any{"type": "object", "required": []any{"mode"}, "properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []any{"read", "write"}}}},
	}, "additionalProperties": false}
	format := map[string]any{"type": "grammar", "syntax": "regex", "definition": "[a-z]+"}
	source := namespaceTestSource("function", "js", "mcp__node_repl")
	clientToolSpecs(source)["mcp__node_repl.js"].Spec["parameters"] = schema
	source["tools"] = append(source["tools"].([]any), map[string]any{"type": "custom", "name": "apply_patch", "format": format})
	catalog := clientToolProtocolInstructions(source)
	for _, want := range []string{"mcp__node_repl.js", string(jsonBytes(schema)), string(jsonBytes(format))} {
		if !strings.Contains(catalog, want) {
			t.Fatalf("tool contract missing from catalog: %s", want)
		}
	}
}

func TestClientToolChoiceConstrainsCallsButNotHistory(t *testing.T) {
	base := namespaceTestSource("function", "js", "alpha")
	base["tools"] = append(base["tools"].([]any), namespaceTestSource("function", "js", "beta")["tools"].([]any)...)
	base["tools"] = append(base["tools"].([]any), map[string]any{"type": "custom", "name": "apply_patch"})
	for _, tc := range []struct {
		name     string
		choice   any
		allowed  []string
		required bool
	}{
		{"default", nil, []string{"alpha.js", "beta.js", "apply_patch"}, false},
		{"auto", "auto", []string{"alpha.js", "beta.js", "apply_patch"}, false},
		{"none", "none", nil, false},
		{"required", "required", []string{"alpha.js", "beta.js", "apply_patch"}, true},
		{"function", map[string]any{"type": "function", "name": "alpha.js"}, []string{"alpha.js"}, true},
		{"namespace-function", map[string]any{"type": "function", "name": "js", "namespace": "beta"}, []string{"beta.js"}, true},
		{"custom", map[string]any{"type": "custom", "name": "apply_patch"}, []string{"apply_patch"}, true},
		{"allowed-auto", map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "name": "alpha.js"}}}, []string{"alpha.js"}, false},
		{"allowed-required", map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}}}, []string{"apply_patch"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := cloneObject(base)
			source["tool_choice"] = tc.choice
			source["parallel_tool_calls"] = false
			allowed := map[string]bool{}
			for _, key := range tc.allowed {
				allowed[key] = true
			}
			if got := callableClientToolSpecs(source); len(got) != len(allowed) {
				t.Fatalf("allowed specs=%v, want=%v", got, allowed)
			}
			catalog := clientToolProtocolInstructions(source)
			if len(allowed) > 0 {
				if tc.choice != nil && !strings.Contains(catalog, "Client tool_choice: "+string(jsonBytes(tc.choice))) {
					t.Fatal("catalog omitted the current tool choice")
				}
				if !strings.Contains(catalog, "at most one client tool") {
					t.Fatal("catalog omitted the parallel call constraint")
				}
			}
			for key, spec := range clientToolSpecs(source) {
				if strings.Contains(catalog, "- "+key+" (") != allowed[key] {
					t.Fatalf("catalog did not restrict %s", key)
				}
				var args any = map[string]any{}
				if spec.Type == "custom" {
					args = "raw input"
				}
				native := namespaceTestNative(t.Name()+"/"+key, key, args)
				_, _, changed, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source)
				if (err == nil) != allowed[key] || changed != allowed[key] {
					t.Fatalf("choice did not constrain %s: changed=%t err=%v", key, changed, err)
				}
			}
			_, _, _, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{messageItem("assistant", "Text only.")}}), source)
			if (err != nil) != tc.required {
				t.Fatalf("required=%t err=%v", tc.required, err)
			}
			historical := map[string]any{"type": "function_call", "name": "js", "namespace": "alpha", "call_id": "uncached_history_" + t.Name(), "arguments": `{}`}
			source["input"] = []any{historical}
			prepared, err := prepareResponsesBody(source, defaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			input := prepared["input"].([]any)
			if envelope, _ := transportEnvelope(objectValue(input[len(input)-1])); envelope["tool"] != "alpha.js" {
				t.Fatalf("tool choice altered history: %#v", input[len(input)-1])
			}
		})
	}
}

func TestRequiredToolChoiceWithoutMatchingToolsIsRejected(t *testing.T) {
	for _, choice := range []any{"required", map[string]any{"type": "function", "name": "missing"}, map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{}}} {
		_, err := prepareResponsesBody(map[string]any{"input": "hello", "tool_choice": choice}, defaultConfig())
		var apiError *APIError
		if !errors.As(err, &apiError) || apiError.Status != 400 || apiError.Kind != "invalid_tool_choice" {
			t.Fatalf("choice=%v err=%v", choice, err)
		}
	}
}
