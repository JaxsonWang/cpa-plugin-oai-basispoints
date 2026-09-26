package basispoints

import (
	"strings"
	"testing"
)

// 服务端原生工具条目不属于本通道契约，原样转发会让上游拒绝整个请求体(422)。
func TestServerNativeItemsAreNotForwardedUpstream(t *testing.T) {
	for _, itemType := range []string{
		"web_search_call",
		"local_shell_call",
		"computer_call",
		"file_search_call",
		"mcp_call",
		"image_generation_call",
	} {
		item := map[string]any{"type": itemType, "id": "srv-1", "status": "completed"}
		if out := translateInputItems([]any{item}, map[string]toolSpec{}); len(out) != 0 {
			t.Fatalf("%s was forwarded upstream: %s", itemType, string(jsonBytes(out)))
		}
	}
}

// 修复不能变成大扫除：消息、图片、密文推理和未知的未来条目都必须保留。
func TestSupportedAndUnknownItemsSurviveSanitization(t *testing.T) {
	dataURL := "data:image/png;base64,AAAA"
	for _, tc := range []struct {
		name string
		item map[string]any
	}{
		{"typed_message", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		{"bare_role_message", map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		{"image_only_message", map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": dataURL}}}},
		{"encrypted_reasoning", map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "abc"}},
		{"compaction_trigger", map[string]any{"type": "compaction_trigger"}},
		{"unknown_future_item", map[string]any{"type": "unknown_future_item", "payload": "keepme"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := translateInputItems([]any{tc.item}, map[string]toolSpec{}); len(out) != 1 {
				t.Fatalf("item dropped: %s", string(jsonBytes(tc.item)))
			}
		})
	}
}

// tool_choice 过滤掉的工具不能误报为"不在目录中"，否则模型无法从诊断中纠正。
func TestToolChoiceFilteredToolReportsAccurateDiagnostic(t *testing.T) {
	declared := []any{
		map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object"}},
	}
	native := rawRelayNative("choice-filtered", "read_file", "{}", []any{"read_file"})

	filtered := map[string]any{"model": DefaultModelID, "tools": declared,
		"tool_choice": map[string]any{"type": "function", "name": "exec_command"}}
	_, err := extractNativeClientToolCallIn(native, callableClientToolSpecs(filtered), clientToolSpecs(filtered))
	if err == nil || !strings.Contains(err.Error(), "tool_not_allowed_by_tool_choice") {
		t.Fatalf("want tool_choice diagnostic, got %v", err)
	}

	absent := map[string]any{"model": DefaultModelID, "tools": []any{declared[0]}}
	_, err = extractNativeClientToolCallIn(native, callableClientToolSpecs(absent), clientToolSpecs(absent))
	if err == nil || !strings.Contains(err.Error(), "tool_not_in_catalog") {
		t.Fatalf("want catalog diagnostic, got %v", err)
	}

	allowed := map[string]any{"model": DefaultModelID, "tools": declared}
	call, err := extractNativeClientToolCallIn(native, callableClientToolSpecs(allowed), clientToolSpecs(allowed))
	if err != nil || stringValue(call["name"]) != "read_file" {
		t.Fatalf("allowed relay rejected: %v", err)
	}
}
