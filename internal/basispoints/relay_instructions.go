package basispoints

import "strings"

// 函数载荷和外层 arguments 是两层不同的 JSON；custom 只有外层需要编码。
const functionRelayEncoding = `For function tools, first JSON-serialize the complete arguments object, then use that JSON text as the outer code string and serialize the outer arguments object. These are two distinct JSON layers: preserve escaped quotes, backslashes, newlines and tabs in string arguments at both layers. The decoded code must itself parse as one JSON object. A function with a single patch, code, cmd or input field still requires the object wrapper named by its schema; raw patch or script text is only valid for a custom tool.`

// 示例只使用当前允许的已知工具和匹配的参数结构，不把固定工具名或类型写进所有请求。
func clientToolRelayExamples(names []string, specs map[string]toolSpec) string {
	const patch = "*** Begin Patch\n*** Add File: hello.js\n+console.log(\"hello\");\n*** End Patch"
	var examples strings.Builder
	for _, name := range names {
		spec := specs[name]
		var arguments map[string]any
		var payload string
		switch {
		case spec.Name == "exec_command" || strings.HasSuffix(spec.Name, "_exec_command"):
			if spec.Type != "function" {
				continue
			}
			arguments = map[string]any{"cmd": "printf '%s\\n' \"hello\""}
		case spec.Name == "apply_patch" || strings.HasSuffix(spec.Name, "_apply_patch"):
			if spec.Type == "custom" {
				payload = patch
			} else {
				arguments = map[string]any{"patch": patch}
			}
		default:
			continue
		}
		if spec.Type == "function" {
			schema := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")
			if schema == nil || !schemaMatches(arguments, schema) {
				continue
			}
			payload = string(jsonBytes(arguments))
		}
		outer := map[string]any{
			"summary": "Run client tool " + name, "extended_summary": "Relay one client tool through the external client",
			"destructive": false, "references": []any{name}, "code": payload,
		}
		examples.WriteString(" Example outer arguments for " + name + " (" + spec.Type + "): " + string(jsonBytes(outer)) + ".")
	}
	return examples.String()
}
