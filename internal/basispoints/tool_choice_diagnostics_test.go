package basispoints

import (
	"errors"
	"strings"
	"testing"
)

func TestToolChoiceDiagnosticPreservesAuthorization(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, namespace := range []string{"", "tools"} {
			for _, catalog := range []string{"top_level", "additional_tools"} {
				for _, mode := range []string{"auto", "none", "forced_other", "allowed_other", "forced_selected", "allowed_selected", "not_in_catalog"} {
					t.Run(kind+"/"+namespace+"/"+catalog+"/"+mode, func(t *testing.T) {
						source := namespaceTestSource(kind, "invoke", namespace)
						other := namespaceTestSource(kind, "other", namespace)
						source["tools"] = append(source["tools"].([]any), other["tools"].([]any)...)
						if catalog == "additional_tools" {
							source["input"] = []any{additionalToolsItem(source["tools"].([]any))}
							delete(source, "tools")
						}
						selected := map[string]any{"type": kind, "name": "invoke"}
						if namespace != "" {
							selected["namespace"] = namespace
						}
						key := "invoke"
						if namespace != "" {
							key = namespace + "." + key
						}
						wantReason := ""
						switch mode {
						case "auto":
							source["tool_choice"] = "auto"
						case "none":
							source["tool_choice"] = "none"
							wantReason = "tool_not_allowed_by_tool_choice"
						case "forced_other", "allowed_other":
							selected["name"] = "other"
							wantReason = "tool_not_allowed_by_tool_choice"
						case "not_in_catalog":
							key = "not_declared"
							wantReason = "tool_not_in_catalog"
						}
						if strings.HasPrefix(mode, "forced_") {
							source["tool_choice"] = selected
						} else if strings.HasPrefix(mode, "allowed_") {
							source["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{selected}}
						}
						native := rawRelayNative(t.Name(), key, `{"value":"PRIVATE"}`, []any{key})
						body, response, changed, err := transformResponseBody(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}), source)
						if wantReason != "" {
							var api *APIError
							if !errors.As(err, &api) || api.Status != 422 || api.Kind != "invalid_tool_call" || !strings.HasSuffix(api.Message, ": "+wantReason) {
								t.Fatalf("want %s diagnostic, got %v", wantReason, err)
							}
							if body != nil || response != nil || changed || rememberedNativeCall(t.Name()) != nil {
								t.Fatal("disallowed call was delivered or cached")
							}
							if strings.Contains(api.Message, "PRIVATE") {
								t.Fatal("diagnostic leaked tool input")
							}
							return
						}
						if err != nil || !changed {
							t.Fatalf("allowed call was rejected: %v", err)
						}
						call := objectValue(response["output"].([]any)[0])
						if call["name"] != "invoke" || stringValue(call["namespace"]) != namespace {
							t.Fatal("allowed tool identity changed")
						}
					})
				}
			}
		}
	}
}
