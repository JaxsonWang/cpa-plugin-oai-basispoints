package basispoints

import (
	"reflect"
	"strings"
	"testing"
)

func additionalToolsItem(tools any) map[string]any {
	return map[string]any{"type": "additional_tools", "id": "at_cli_fixture", "role": "developer", "tools": tools}
}

func TestAdditionalToolsCatalogAndHistory(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		t.Run(kind, func(t *testing.T) {
			tools := namespaceTestSource(kind, "exec", "functions")["tools"]
			user := messageItem("user", "Run the read-only probe.")
			source := map[string]any{"model": DefaultModelID, "tool_choice": "required", "input": []any{additionalToolsItem(tools), user}}
			before := string(jsonBytes(source))
			specs := clientToolSpecs(source)
			if len(specs) != 1 || specs["functions.exec"].Type != kind {
				t.Fatalf("missing CLI tool: %#v", specs)
			}
			prepared, err := prepareResponsesBody(source, defaultConfig())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := prepared["tools"]; ok {
				t.Fatal("native tools leaked upstream")
			}
			for _, value := range prepared["input"].([]any) {
				if objectValue(value)["type"] == "additional_tools" {
					t.Fatal("tool declaration leaked upstream")
				}
			}
			if catalog := clientToolProtocolInstructions(source); !strings.Contains(catalog, "- functions.exec ("+kind+")") {
				t.Fatalf("catalog missing definition: %s", catalog)
			}
			if !strings.Contains(clientToolProtocolReminder(source), "functions.exec") {
				t.Fatal("reminder omitted tool")
			}
			var args any = map[string]any{"cmd": "printf PROBE_OK"}
			field, callType, outputType := "arguments", "function_call", "function_call_output"
			if kind == "custom" {
				args = "const r = await tools.exec_command({cmd: 'printf PROBE_OK'}); text(r);"
				field, callType, outputType = "input", "custom_tool_call", "custom_tool_call_output"
			}
			native := namespaceTestNative(t.Name(), "functions.exec", args)
			_, response, changed, err := transformResponseBody(jsonBytes(map[string]any{"id": "resp_cli_fixture", "status": "completed", "output": []any{native}}), source)
			if err != nil || !changed {
				t.Fatalf("transform: changed=%t err=%v", changed, err)
			}
			call := objectValue(response["output"].([]any)[0])
			if call["type"] != callType || call["name"] != "exec" || call["namespace"] != "functions" {
				t.Fatalf("identity lost: %#v", call)
			}
			want := args
			if kind == "function" {
				want = string(jsonBytes(args))
			}
			if call[field] != want {
				t.Fatalf("input changed: %#v", call)
			}
			events := clientStreamEvents(t, syntheticStream(response))
			if events[len(events)-1]["type"] != "response.completed" {
				t.Fatal("stream has no completed response")
			}
			for _, cached := range []bool{true, false} {
				historical := cloneObject(call)
				if !cached {
					historical["call_id"] = stringValue(call["call_id"]) + "_uncached"
				}
				result := map[string]any{"type": outputType, "call_id": historical["call_id"], "output": "PROBE_OK"}
				history := []any{additionalToolsItem(tools), user, historical, result}
				replay := translateInputItems(history)
				if len(replay) != 3 || !reflect.DeepEqual(replay[0], user) {
					t.Fatalf("history changed: %#v", replay)
				}
				envelope, err := transportEnvelope(objectValue(replay[1]))
				if err != nil || envelope["tool"] != "functions.exec" || envelope["args"] != relayTestPayload(args) {
					t.Fatalf("invalid replay: %#v err=%v", envelope, err)
				}
				if output := objectValue(replay[2]); output["type"] != "function_call_output" || output["output"] != "PROBE_OK" {
					t.Fatalf("result changed: %#v", output)
				}
			}
			if string(jsonBytes(source)) != before {
				t.Fatal("request mutated")
			}
			source["tool_choice"] = "none"
			if len(callableClientToolSpecs(source)) != 0 || strings.Contains(clientToolProtocolInstructions(source), "- functions.exec (") {
				t.Fatal("tool_choice=none ignored")
			}
			if _, _, _, err := transformResponseBody(jsonBytes(map[string]any{"output": []any{native}}), source); err == nil {
				t.Fatal("disabled tool accepted")
			}
		})
	}
}

func TestAdditionalToolsMergeAndCatalogConsistency(t *testing.T) {
	base := namespaceTestSource("function", "exec", "functions")
	replacement := namespaceTestSource("custom", "exec", "functions")
	latest := objectValue(objectValue(replacement["tools"].([]any)[0])["tools"].([]any)[0])
	latest["description"] = "LATEST_DEFINITION"
	extra := namespaceTestSource("function", "sleep", "clock")
	base["input"] = []any{
		additionalToolsItem(extra["tools"]),
		additionalToolsItem(replacement["tools"]),
		map[string]any{"type": "message", "role": "user", "tools": namespaceTestSource("function", "fake", "")["tools"], "content": "not a declaration"},
	}
	specs := clientToolSpecs(base)
	if len(specs) != 2 || specs["functions.exec"].Type != "custom" || specs["clock.sleep"].Type != "function" {
		t.Fatalf("merged specs=%#v", specs)
	}
	catalog := clientToolProtocolInstructions(base)
	if strings.Count(catalog, "- functions.exec (") != 1 || !strings.Contains(catalog, "LATEST_DEFINITION") || strings.Contains(catalog, "- fake (") {
		t.Fatalf("catalog disagrees with specs: %s", catalog)
	}
	base["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "custom", "namespace": "functions", "name": "exec"}}}
	if allowed := callableClientToolSpecs(base); len(allowed) != 1 || allowed["functions.exec"].Type != "custom" {
		t.Fatalf("allowed specs=%#v", allowed)
	}
	if strings.Contains(clientToolProtocolInstructions(base), "- clock.sleep (") {
		t.Fatal("excluded tool advertised")
	}
}
