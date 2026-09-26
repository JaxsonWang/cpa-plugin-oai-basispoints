package basispoints

import (
	"strings"
	"testing"
)

// 上游偶发的 code 字段编码缺陷不应让整回合 422；修复层只做无损恢复。
func TestRelayPayloadRepairsRecoverableEncodingFaults(t *testing.T) {
	pad := strings.Repeat("a", 800)
	for _, tc := range []struct {
		name    string
		payload string
		cmd     string
	}{
		{"raw_newline_in_string", "{\"cmd\":\"" + pad + "\n tail\"}", pad + "\n tail"},
		{"raw_tab_in_string", "{\"cmd\":\"" + pad + "\t tail\"}", pad + "\t tail"},
		{"markdown_fence", "```json\n{\"cmd\":\"x\"}\n```", "x"},
		{"trailing_comma", "{\"cmd\":\"x\",}", "x"},
		{"double_encoded", "\"{\\\"cmd\\\":\\\"x\\\"}\"", "x"},
		{"already_valid_is_untouched", "{\"cmd\":\"printf \\\"hi\\\"\"}", "printf \"hi\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object, reason := parseRelayPayload(tc.payload)
			if reason != "" {
				t.Fatalf("recoverable payload rejected: %s", reason)
			}
			if stringValue(object["cmd"]) != tc.cmd {
				t.Fatal("repair changed payload bytes")
			}
		})
	}
}

// 真正缺失结构的载荷仍必须失败，不允许猜测补引号。
func TestRelayPayloadKeepsRejectingUnrecoverableJSON(t *testing.T) {
	for _, payload := range []string{
		"{\"cmd\":\"" + strings.Repeat("a", 800) + "\"tail\"}",
		"{\"cmd\":",
		"[]",
		"null",
		"{} {}",
	} {
		if _, reason := parseRelayPayload(payload); reason == "" {
			t.Fatalf("unrecoverable payload accepted: %q", payload)
		}
	}
}
