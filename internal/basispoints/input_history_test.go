package basispoints

import (
	"reflect"
	"testing"
)

func TestInputHistoryPreservesNativeToolResults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []any
	}{
		{"mcp_result", []any{
			map[string]any{"type": "mcp_call", "id": "mcp_prior", "name": "read_build_status", "server_label": "ci", "arguments": "{}", "status": "completed", "output": "Build failed; do not deploy."},
		}},
		{"shell_result", []any{
			map[string]any{"type": "local_shell_call", "id": "lsc_prior", "call_id": "call_prior", "status": "completed", "action": map[string]any{"type": "exec", "command": []any{"printf", "Migration already ran."}, "env": map[string]any{}}},
			map[string]any{"type": "local_shell_call_output", "id": "lso_prior", "call_id": "call_prior", "output": "Migration already ran; do not repeat."},
		}},
		{"computer_result", []any{
			map[string]any{"type": "computer_call", "id": "cu_prior", "call_id": "call_computer", "status": "completed", "action": map[string]any{"type": "screenshot"}, "pending_safety_checks": []any{}},
			map[string]any{"type": "computer_call_output", "call_id": "call_computer", "output": map[string]any{"type": "computer_screenshot", "image_url": "https://example.invalid/prior-result.png"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := []any{messageItem("user", "Check the result before continuing.")}
			history = append(history, tc.items...)
			history = append(history, messageItem("user", "Continue from that tool result."))
			source := map[string]any{"model": DefaultModelID, "input": history}
			before := string(jsonBytes(source))
			body, err := prepareResponsesBody(source, defaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			items := body["input"].([]any)
			// 只验证不静默删除原有历史，不声明上游支持这些原生类型。
			if len(items) < len(history) || !reflect.DeepEqual(items[len(items)-len(history):], history) {
				t.Fatal("historical tool calls or results were silently changed")
			}
			if string(jsonBytes(source)) != before {
				t.Fatal("client history was mutated")
			}
		})
	}
}

func TestInputHistoryPreservesSupportedAndUnknownItems(t *testing.T) {
	for _, tc := range []struct {
		name string
		item map[string]any
	}{
		{"typed_message", messageItem("user", "hi")},
		{"bare_role_message", map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		{"image_only_message", map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}}}},
		{"encrypted_reasoning", map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "abc"}},
		{"compaction_trigger", map[string]any{"type": "compaction_trigger"}},
		{"unknown_future_item", map[string]any{"type": "unknown_future_item", "payload": "keepme"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := translateInputItems([]any{tc.item}); len(out) != 1 || !reflect.DeepEqual(out[0], tc.item) {
				t.Fatal("input history was dropped or changed")
			}
		})
	}
}
