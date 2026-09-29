package basispoints

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// 从实际发送的提示词读取示例，再通过生产解析路径校验，避免只断言提示词片段。
func relayInstructionExamples(t *testing.T, instructions string) []map[string]any {
	t.Helper()
	markers := regexp.MustCompile(`Example outer arguments [^\n:]+: `).FindAllStringIndex(instructions, -1)
	examples := make([]map[string]any, 0, len(markers))
	for _, marker := range markers {
		var arguments map[string]any
		if err := json.NewDecoder(strings.NewReader(instructions[marker[1]:])).Decode(&arguments); err != nil {
			t.Fatalf("invalid outer example JSON: %v", err)
		}
		examples = append(examples, arguments)
	}
	return examples
}

func TestRelayInstructionExamplesMatchCallableCatalog(t *testing.T) {
	function := func(name, field string) map[string]any {
		return map[string]any{"type": "function", "name": name, "parameters": map[string]any{
			"type": "object", "required": []any{field}, "additionalProperties": false,
			"properties": map[string]any{field: map[string]any{"type": "string"}},
		}}
	}
	custom := map[string]any{"type": "custom", "name": "apply_patch"}
	for _, tc := range []struct {
		name   string
		source map[string]any
		count  int
	}{
		{"shell_only", map[string]any{"tools": []any{function("exec_command", "cmd")}}, 1},
		{"raw_patch_only", map[string]any{"tools": []any{custom}}, 1},
		{"function_patch", map[string]any{"tools": []any{function("apply_patch", "patch")}}, 1},
		{"mixed_patch_types", map[string]any{"tools": []any{custom, map[string]any{
			"type": "namespace", "name": "mcp__fixture", "tools": []any{function("_apply_patch", "patch")},
		}}}, 2},
		{"namespaced_function_patch", map[string]any{"tools": []any{map[string]any{
			"type": "namespace", "name": "mcp__fixture", "tools": []any{function("_apply_patch", "patch")},
		}}}, 1},
		{"different_schema", map[string]any{"tools": []any{function("exec_command", "request")}}, 0},
		{"unrelated_tool", map[string]any{"tools": []any{function("echo", "text")}}, 0},
		{"forced_shell", map[string]any{"tools": []any{function("exec_command", "cmd"), custom}, "tool_choice": map[string]any{"type": "function", "name": "exec_command"}}, 1},
		{"disabled", map[string]any{"tools": []any{function("exec_command", "cmd"), custom}, "tool_choice": "none"}, 0},
		{"dynamic_override", map[string]any{"tools": []any{custom}, "input": []any{map[string]any{
			"type": "additional_tools", "tools": []any{function("apply_patch", "patch")},
		}}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instructions := clientToolProtocolInstructions(tc.source)
			examples := relayInstructionExamples(t, instructions)
			if len(examples) != tc.count {
				t.Errorf("example count=%d want=%d", len(examples), tc.count)
			}
			for index, arguments := range examples {
				native := map[string]any{"type": "function_call", "name": transportName,
					"call_id": fmt.Sprintf("%s-%d", t.Name(), index), "arguments": string(jsonBytes(arguments))}
				call, err := extractNativeClientToolCallIn(native, callableClientToolSpecs(tc.source), clientToolSpecs(tc.source))
				if err != nil {
					t.Errorf("instructions teach a rejected relay: %v", err)
					continue
				}
				if call["type"] == "function_call" {
					parsed, reason := parseRelayObject(call["arguments"])
					if reason != "" || len(parsed) != 1 {
						t.Fatal("function example lost its single arguments object")
					}
				} else if !strings.HasPrefix(stringValue(call["input"]), "*** Begin Patch\n") {
					t.Fatal("custom patch example gained an extra JSON layer")
				}
			}
		})
	}
}

func TestFunctionRelayComplexTextRoundTrip(t *testing.T) {
	payload := strings.Repeat("中文🙂\r\n\tconst value = {path: \"C:\\new\\test\", re: /\\s+/};  \n", 36)
	for _, field := range []string{"patch", "code", "cmd", "input"} {
		t.Run(field, func(t *testing.T) {
			source := namespaceTestSource("function", "fixture", "tools")
			native := rawRelayNative(t.Name(), "tools.fixture", string(jsonBytes(map[string]any{field: payload})), []any{"tools.fixture"})
			_, response, _, err := transformResponseBody(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}), source)
			if err != nil {
				t.Fatal(err)
			}
			call := objectValue(response["output"].([]any)[0])
			arguments, reason := parseRelayObject(call["arguments"])
			if reason != "" || arguments[field] != payload {
				t.Fatal("function text changed after decoding both JSON layers")
			}
			var deltas strings.Builder
			for _, event := range clientStreamEvents(t, syntheticStream(response)) {
				if event["type"] == "response.function_call_arguments.delta" {
					deltas.WriteString(stringValue(event["delta"]))
				}
			}
			streamed, reason := parseRelayObject(deltas.String())
			if reason != "" || streamed[field] != payload {
				t.Fatal("streaming changed quotes, escapes or whitespace")
			}
			replay := translateInputItems([]any{call})
			replayed, err := extractNativeClientToolCall(objectValue(replay[0]), clientToolSpecs(source))
			if err != nil || replayed["arguments"] != call["arguments"] {
				t.Fatal("history replay changed the function payload")
			}
		})
	}
}

func TestMalformedFunctionRelayByteOffset706(t *testing.T) {
	// 合成同偏移输入，不冒充原会话未留存的上游报文。
	const prefix = `{"patch":"`
	code := prefix + strings.Repeat("x", 706-len(prefix)-2) + `"q"}`
	source := namespaceTestSource("function", "apply_patch", "")
	native := rawRelayNative(t.Name(), "apply_patch", code, []any{"apply_patch"})
	_, err := extractNativeClientToolCall(native, clientToolSpecs(source))
	api, ok := err.(*APIError)
	if !ok || api.Status != 422 || api.Kind != "invalid_tool_call" || api.Message != "Basis Points returned an invalid client tool relay: code invalid_json byte_offset=706" {
		t.Fatalf("malformed function JSON was not strictly rejected: %v", err)
	}
	if strings.Contains(api.Message, "patch") || strings.Contains(api.Message, "xxxx") {
		t.Fatal("error leaked the rejected payload")
	}
}
