package basispoints

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCustomToolCallItemID(t *testing.T) {
	for _, withID := range []bool{true, false} {
		native := namespaceTestNative("custom_id", "functions.exec", "  text('ok');\r\n")
		wantID := "ctc_custom_id"
		if !withID {
			delete(native, "id")
			wantID = "ctc_call_custom_id"
		}
		before := string(jsonBytes(native))
		call, err := extractNativeClientToolCall(native, clientToolSpecs(namespaceTestSource("custom", "exec", "functions")))
		if err != nil {
			t.Fatal(err)
		}
		if call["id"] != wantID || call["call_id"] != native["call_id"] || call["type"] != "custom_tool_call" {
			t.Errorf("withID=%t: identity = %#v, want id=%s and unchanged call_id", withID, call, wantID)
		}
		if string(jsonBytes(native)) != before {
			t.Fatal("native call was mutated")
		}
	}
}

func TestHistoryReplayAfterNativeCacheEviction(t *testing.T) {
	source := namespaceTestSource("custom", "exec", "functions")
	native := namespaceTestNative(t.Name(), "functions.exec", "  text('done');\r\n")
	_, response, _, err := transformResponseBody(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}), source)
	if err != nil {
		t.Fatal(err)
	}
	call := objectValue(response["output"].([]any)[0])
	result := map[string]any{"type": "custom_tool_call_output", "call_id": call["call_id"], "output": "already executed"}
	history := []any{call, result}
	if replay := translateInputItems(history); !reflect.DeepEqual(replay[0], native) {
		t.Fatal("warm replay lost the original native call")
	}
	for i := 0; i < 513; i++ {
		rememberNativeCall(namespaceTestNative(t.Name()+strconv.Itoa(i), "functions.exec", "text('unrelated');"))
	}
	if rememberedNativeCall(stringValue(call["call_id"])) != nil {
		t.Fatal("expected the original native call to be evicted")
	}
	replay := translateInputItems(history)
	replayedCall, replayedOutput := objectValue(replay[0]), objectValue(replay[1])
	envelope, err := transportEnvelope(replayedCall)
	if err != nil || envelope["tool"] != "functions.exec" || envelope["args"] != call["input"] {
		t.Fatalf("evicted history was not restored: %#v: %v", replayedCall, err)
	}
	if replayedCall["call_id"] != call["call_id"] || replayedOutput["call_id"] != call["call_id"] || replayedOutput["type"] != "function_call_output" || replayedOutput["output"] != result["output"] {
		t.Fatalf("evicted history lost result pairing: %#v", replay)
	}
}

func TestHistoryReplayWithoutCurrentToolCatalog(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		for _, namespace := range []string{"", "functions"} {
			for _, catalog := range []string{"absent", "empty", "unrelated"} {
				t.Run(kind+"/"+namespace+"/"+catalog, func(t *testing.T) {
					callID := "call_" + t.Name()
					call := map[string]any{"type": "function_call", "id": "fc_old", "call_id": callID, "name": "exec", "arguments": `{"cmd":"printf OK","id":9007199254740993}`}
					outputType, field := "function_call_output", "arguments"
					if kind == "custom" {
						// Old plugin versions saved custom calls with an fc_ item ID.
						call["type"], call["input"] = "custom_tool_call", "  text(\"already executed\");\r\n\t"
						delete(call, "arguments")
						outputType, field = "custom_tool_call_output", "input"
					}
					key := "exec"
					if namespace != "" {
						call["namespace"] = namespace
						key = namespace + ".exec"
					}
					output := []any{map[string]any{"type": "input_text", "text": "  already executed\n"}}
					result := map[string]any{"type": outputType, "id": "ctco_old", "call_id": callID, "name": "exec", "namespace": namespace, "output": output}
					source := map[string]any{"model": DefaultModelID, "input": []any{call, result, messageItem("user", "Summarize this conversation.")}}
					if catalog == "empty" {
						source["tools"] = []any{}
					} else if catalog == "unrelated" {
						source["tools"] = namespaceTestSource("function", "other", "")["tools"]
					}
					if rememberedNativeCall(callID) != nil {
						t.Fatal("expected a cold cache")
					}
					before := string(jsonBytes(source))
					body, err := prepareResponsesBody(source, defaultConfig())
					if err != nil {
						t.Fatal(err)
					}
					items := body["input"].([]any)
					replayedCall := objectValue(items[len(items)-3])
					envelope, err := transportEnvelope(replayedCall)
					if err != nil {
						t.Fatalf("history was not restored to native transport: %#v: %v", replayedCall, err)
					}
					if envelope["tool"] != key || envelope["args"] != call[field] || replayedCall["call_id"] != callID || !strings.HasPrefix(stringValue(replayedCall["id"]), "fc_") {
						t.Fatalf("history identity or payload changed: %#v", replayedCall)
					}
					replayedOutput := objectValue(items[len(items)-2])
					if replayedOutput["type"] != "function_call_output" || replayedOutput["call_id"] != callID || !reflect.DeepEqual(replayedOutput["output"], output) {
						t.Fatalf("history output changed: %#v", replayedOutput)
					}
					for _, key := range []string{"name", "namespace"} {
						if _, exists := replayedOutput[key]; exists {
							t.Fatalf("client %s leaked into output", key)
						}
					}
					if string(jsonBytes(source)) != before {
						t.Fatal("history was mutated")
					}
				})
			}
		}
	}
}
