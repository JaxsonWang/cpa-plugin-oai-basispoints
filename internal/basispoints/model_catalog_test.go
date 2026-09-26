package basispoints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func catalogFixture(canonical, alias string) []byte {
	return jsonBytes(map[string]any{
		"future_counter": json.Number("9007199254740993"),
		"models": []any{
			map[string]any{"slug": canonical, "apply_patch_tool_type": "freeform", "context_window": 272000, "max_context_window": 872000, "effective_context_window_percent": 95, "service_tiers": []any{map[string]any{"id": "priority", "description": "2x speed"}}},
			map[string]any{"slug": alias, "context_window": 272000, "max_context_window": 272000, "service_tiers": []any{}, "additional_speed_tiers": []string{"fast"}, "base_instructions": "keep alias instructions", "supported_reasoning_levels": []string{"ultra"}, "future_field": map[string]any{"counter": json.Number("9007199254740993")}},
			map[string]any{"slug": "unrelated-basispoints", "context_window": 1000, "max_context_window": 1000, "service_tiers": []any{}},
		},
	})
}

func catalogRequest(body []byte) catalogInterceptRequest {
	return catalogInterceptRequest{SourceFormat: "openai", StatusCode: 200, Body: body}
}

func decodedCatalog(t *testing.T, raw []byte) (map[string]json.RawMessage, []map[string]json.RawMessage) {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(root["models"], &models); err != nil {
		t.Fatal(err)
	}
	return root, models
}

func TestModelCatalogRepairsLegacyHostResponse(t *testing.T) {
	svc := NewService()
	before := catalogFixture(DefaultUpstreamModel, DefaultModelID)
	result, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(before)))
	if err != nil {
		t.Fatal(err)
	}
	// 按旧宿主真实 JSON ABI 再编码、解码，确认 Body 仍是字节数组。
	var reply struct{ Body []byte }
	if err := json.Unmarshal(jsonBytes(result), &reply); err != nil {
		t.Fatal(err)
	}
	root, models := decodedCatalog(t, reply.Body)
	original, oldModels := decodedCatalog(t, before)
	if !reflect.DeepEqual(models[0], oldModels[0]) || !reflect.DeepEqual(models[2], oldModels[2]) {
		t.Fatal("changed another model")
	}
	if string(root["future_counter"]) != string(original["future_counter"]) {
		t.Fatal("lost large integer precision")
	}
	for _, field := range []string{"context_window", "max_context_window", "effective_context_window_percent", "apply_patch_tool_type"} {
		if string(models[1][field]) != string(models[0][field]) {
			t.Fatalf("canonical field not applied: %s", field)
		}
	}
	for _, field := range []string{"slug", "base_instructions", "supported_reasoning_levels", "future_field"} {
		if !reflect.DeepEqual(models[1][field], oldModels[1][field]) {
			t.Fatalf("changed alias field: %s", field)
		}
	}
	var tiers []struct {
		ID          string
		Name        string
		Description string
	}
	if err := json.Unmarshal(models[1]["service_tiers"], &tiers); err != nil {
		t.Fatal(err)
	}
	if len(tiers) != 0 {
		t.Fatalf("unsupported speed tiers advertised: %v", tiers)
	}
	if string(models[1]["additional_speed_tiers"]) != "[]" {
		t.Fatal("Fast still advertised")
	}
	again, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(reply.Body)))
	if err != nil || len(again.(map[string]any)) != 0 {
		t.Fatalf("not idempotent: %v %v", again, err)
	}
}

func TestModelCatalogIgnoresOtherResponses(t *testing.T) {
	for _, name := range []string{"format", "status", "stream", "model", "requested-model", "original-request", "request-body", "ordinary-list", "invalid-body", "empty-catalog", "unrelated-models"} {
		t.Run(name, func(t *testing.T) {
			req := catalogRequest(catalogFixture(DefaultUpstreamModel, DefaultModelID))
			switch name {
			case "format":
				req.SourceFormat = "claude"
			case "status":
				req.StatusCode = 500
			case "stream":
				req.Stream = true
			case "model":
				req.Model = DefaultModelID
			case "requested-model":
				req.RequestedModel = DefaultModelID
			case "original-request":
				req.OriginalRequest = []byte(`{}`)
			case "request-body":
				req.RequestBody = []byte(`{}`)
			case "ordinary-list":
				req.Body = []byte(`{"object":"list","data":[{"id":"gpt-6-astra-basispoints"}]}`)
			case "invalid-body":
				req.Body = []byte(`not-json`)
			case "empty-catalog":
				req.Body = []byte(`{"models":[]}`)
			case "unrelated-models":
				req.Body = catalogFixture(DefaultUpstreamModel, "not-our-alias")
			}
			result, err := NewService().Handle("response.intercept_after", jsonBytes(req))
			if err != nil || len(result.(map[string]any)) != 0 {
				t.Fatalf("unrelated response changed: %v %v", result, err)
			}
		})
	}
}

func TestModelCatalogRequiresRealCanonicalMetadata(t *testing.T) {
	for _, field := range []string{"missing-model", "context_window", "max_context_window"} {
		t.Run(field, func(t *testing.T) {
			root, models := decodedCatalog(t, catalogFixture(DefaultUpstreamModel, DefaultModelID))
			if field == "missing-model" {
				models = models[1:]
			} else {
				models[0][field] = json.RawMessage(`0`)
			}
			root["models"] = jsonBytes(models)
			result, err := NewService().Handle("response.intercept_after", jsonBytes(catalogRequest(jsonBytes(root))))
			if result != nil || err == nil {
				t.Fatal("missing metadata was silently fabricated")
			}
			apiErr, ok := err.(*APIError)
			if !ok || apiErr.Kind != "model_metadata_missing" {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestModelCatalogUsesConfiguredNamesAndPrefixes(t *testing.T) {
	for _, prefix := range []string{"", "tenant/"} {
		svc := NewService()
		svc.cfg.UpstreamModel = "custom-canonical"
		svc.cfg.Models = []string{"custom-alias"}
		body := catalogFixture(prefix+svc.cfg.UpstreamModel, prefix+svc.cfg.Models[0])
		result, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(body)))
		if err != nil {
			t.Fatal(err)
		}
		_, models := decodedCatalog(t, result.(map[string]any)["Body"].([]byte))
		if string(models[1]["apply_patch_tool_type"]) != `"freeform"` {
			t.Fatal("configured alias patch tool not restored")
		}
		if string(models[1]["max_context_window"]) != "872000" {
			t.Fatal("configured alias not repaired")
		}
	}
}

func TestModelCatalogDoesNotRetainGenericEffectivePercentage(t *testing.T) {
	root, models := decodedCatalog(t, catalogFixture(DefaultUpstreamModel, DefaultModelID))
	delete(models[0], "effective_context_window_percent")
	models[1]["effective_context_window_percent"] = json.RawMessage(`80`)
	root["models"] = jsonBytes(models)
	result, err := NewService().Handle("response.intercept_after", jsonBytes(catalogRequest(jsonBytes(root))))
	if err != nil {
		t.Fatal(err)
	}
	_, updated := decodedCatalog(t, result.(map[string]any)["Body"].([]byte))
	if _, exists := updated[1]["effective_context_window_percent"]; exists {
		t.Fatal("retained stale generic percentage")
	}
}

func TestModelCatalogRoutesEachMappedAlias(t *testing.T) {
	for _, prefix := range []string{"", "tenant/"} {
		t.Run(prefix, func(t *testing.T) {
			svc := NewService()
			svc.cfg = mappedConfig(3)
			var entries []any
			for i, alias := range svc.cfg.Models {
				canonical := map[string]any{"slug": prefix + svc.cfg.ModelMappings[alias], "context_window": 10000 + i*1000, "max_context_window": 20000 + i*1000}
				if i%2 == 0 {
					canonical["effective_context_window_percent"] = 90 + i
				}
				switch i {
				case 0:
					canonical["apply_patch_tool_type"] = "freeform"
					canonical["multi_agent_version"] = "v2"
					canonical["multi_agent_reasoning_effort"] = "xhigh"
					canonical["experimental_supported_tools"] = []string{"clock"}
				case 1:
					canonical["apply_patch_tool_type"] = nil
					canonical["multi_agent_version"] = "v1"
					canonical["experimental_supported_tools"] = []string{"send_user_message_async"}
				}
				entries = append(entries, canonical, map[string]any{"slug": prefix + alias, "apply_patch_tool_type": "freeform", "context_window": 1, "max_context_window": 1, "effective_context_window_percent": 1, "base_instructions": "keep alias instructions"})
			}
			before := jsonBytes(map[string]any{"models": entries})
			result, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(before)))
			if err != nil {
				t.Fatal(err)
			}
			_, old := decodedCatalog(t, before)
			_, updated := decodedCatalog(t, result.(map[string]any)["Body"].([]byte))
			for i := range svc.cfg.Models {
				canonical, alias := i*2, i*2+1
				if !reflect.DeepEqual(old[canonical], updated[canonical]) {
					t.Fatal("native model metadata changed")
				}
				for _, field := range []string{"context_window", "max_context_window", "effective_context_window_percent", "apply_patch_tool_type"} {
					if !reflect.DeepEqual(updated[alias][field], old[canonical][field]) {
						t.Fatalf("model %d has another model's %s", i, field)
					}
				}
				wantTools := old[canonical]["experimental_supported_tools"]
				if wantTools == nil {
					wantTools = json.RawMessage(`[]`)
				}
				if !reflect.DeepEqual(updated[alias]["experimental_supported_tools"], wantTools) {
					t.Fatalf("model %d inherited another model's experimental tools", i)
				}
				for _, field := range []string{"slug", "base_instructions"} {
					if !reflect.DeepEqual(old[alias][field], updated[alias][field]) {
						t.Fatalf("alias field changed: %s", field)
					}
				}
			}
		})
	}
}

func TestModelCatalogMappedAliasRequiresOwnCanonical(t *testing.T) {
	for _, prefix := range []string{"", "tenant/"} {
		svc := NewService()
		svc.cfg = mappedConfig(3)
		alias := svc.cfg.Models[1]
		entries := []any{
			map[string]any{"slug": prefix + DefaultUpstreamModel, "context_window": 10000, "max_context_window": 20000},
			map[string]any{"slug": prefix + svc.cfg.ModelMappings[svc.cfg.Models[0]], "context_window": 30000, "max_context_window": 40000},
			map[string]any{"slug": prefix + alias, "context_window": 1, "max_context_window": 1},
		}
		if prefix != "" {
			entries = append(entries, map[string]any{"slug": svc.cfg.ModelMappings[alias], "context_window": 50000, "max_context_window": 60000})
		}
		result, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(jsonBytes(map[string]any{"models": entries}))))
		apiErr, ok := err.(*APIError)
		if result != nil || !ok || apiErr.Kind != "model_metadata_missing" || !strings.Contains(err.Error(), prefix+svc.cfg.ModelMappings[alias]) {
			t.Fatalf("missing mapped canonical was not reported: %v", err)
		}
	}
}

func TestModelCatalogMatchesCanonicalPatchCapability(t *testing.T) {
	states := []struct {
		name  string
		value json.RawMessage
	}{
		{"missing", nil},
		{"null", json.RawMessage(`null`)},
		{"freeform", json.RawMessage(`"freeform"`)},
	}
	for _, canonical := range states {
		for _, alias := range states {
			t.Run(canonical.name+"/"+alias.name, func(t *testing.T) {
				root, models := decodedCatalog(t, catalogFixture(DefaultUpstreamModel, DefaultModelID))
				delete(models[0], "apply_patch_tool_type")
				if canonical.value != nil {
					models[0]["apply_patch_tool_type"] = canonical.value
				}
				if alias.value != nil {
					models[1]["apply_patch_tool_type"] = alias.value
				}
				models[2]["apply_patch_tool_type"] = json.RawMessage(`"freeform"`)
				root["models"] = jsonBytes(models)
				svc := NewService()
				result, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(jsonBytes(root))))
				if err != nil {
					t.Fatal(err)
				}
				body := result.(map[string]any)["Body"].([]byte)
				_, updated := decodedCatalog(t, body)
				if !reflect.DeepEqual(updated[1]["apply_patch_tool_type"], canonical.value) {
					t.Fatalf("alias patch capability = %s, want %s", updated[1]["apply_patch_tool_type"], canonical.value)
				}
				if !reflect.DeepEqual(models[0], updated[0]) || !reflect.DeepEqual(models[2], updated[2]) {
					t.Fatal("changed another model's capabilities")
				}
				again, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(body)))
				if err != nil || len(again.(map[string]any)) != 0 {
					t.Fatalf("not idempotent: %v %v", again, err)
				}
			})
		}
	}
}

func TestModelCatalogRestoresOnlySupportedClientCapabilities(t *testing.T) {
	root, models := decodedCatalog(t, catalogFixture(DefaultUpstreamModel, DefaultModelID))
	models[0]["multi_agent_version"] = json.RawMessage(`"v2"`)
	models[0]["multi_agent_reasoning_effort"] = json.RawMessage(`"xhigh"`)
	models[0]["experimental_supported_tools"] = json.RawMessage(`["clock","unknown_future_tool","send_user_message_async"]`)
	models[0]["tool_mode"] = json.RawMessage(`"code_mode_only"`)
	models[0]["node_repl_auto_review_required"] = json.RawMessage(`true`)
	models[1]["node_repl_auto_review_required"] = json.RawMessage(`false`)
	models[1]["experimental_supported_tools"] = json.RawMessage(`["stale_tool"]`)
	root["models"] = jsonBytes(models)
	svc := NewService()
	result, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(jsonBytes(root))))
	if err != nil {
		t.Fatal(err)
	}
	body := result.(map[string]any)["Body"].([]byte)
	_, updated := decodedCatalog(t, body)
	for _, field := range []string{"multi_agent_version", "multi_agent_reasoning_effort"} {
		if _, exists := updated[1][field]; exists {
			t.Fatalf("unimplemented encrypted capability advertised: %s", field)
		}
	}
	if string(updated[1]["experimental_supported_tools"]) != `["clock","send_user_message_async"]` {
		t.Fatalf("unexpected client tools: %s", updated[1]["experimental_supported_tools"])
	}
	if _, exists := updated[1]["tool_mode"]; exists || string(updated[1]["node_repl_auto_review_required"]) != "false" {
		t.Fatal("changed tool execution mode or approval policy")
	}
	if !reflect.DeepEqual(updated[0], models[0]) || !reflect.DeepEqual(updated[2], models[2]) {
		t.Fatal("changed unrelated models")
	}
	again, err := svc.Handle("response.intercept_after", jsonBytes(catalogRequest(body)))
	if err != nil || len(again.(map[string]any)) != 0 {
		t.Fatalf("not idempotent: %v %v", again, err)
	}
}

func TestModelCatalogClearsUndeclaredClientCapabilities(t *testing.T) {
	for _, declaration := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`)} {
		root, models := decodedCatalog(t, catalogFixture(DefaultUpstreamModel, DefaultModelID))
		if declaration != nil {
			models[0]["experimental_supported_tools"] = declaration
		}
		models[1]["experimental_supported_tools"] = json.RawMessage(`["clock"]`)
		models[1]["multi_agent_version"] = json.RawMessage(`"v2"`)
		models[1]["multi_agent_reasoning_effort"] = json.RawMessage(`"xhigh"`)
		root["models"] = jsonBytes(models)
		result, err := NewService().Handle("response.intercept_after", jsonBytes(catalogRequest(jsonBytes(root))))
		if err != nil {
			t.Fatal(err)
		}
		_, updated := decodedCatalog(t, result.(map[string]any)["Body"].([]byte))
		if string(updated[1]["experimental_supported_tools"]) != "[]" {
			t.Fatal("retained undeclared experimental tools")
		}
		for _, field := range []string{"multi_agent_version", "multi_agent_reasoning_effort"} {
			if _, exists := updated[1][field]; exists {
				t.Fatalf("retained undeclared capability: %s", field)
			}
		}
	}
}

func TestModelCatalogRejectsInvalidExperimentalToolMetadata(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`"clock"`), json.RawMessage(`[true]`), json.RawMessage(`{"private-value":1}`)} {
		root, models := decodedCatalog(t, catalogFixture(DefaultUpstreamModel, DefaultModelID))
		models[0]["experimental_supported_tools"] = raw
		root["models"] = jsonBytes(models)
		result, err := NewService().Handle("response.intercept_after", jsonBytes(catalogRequest(jsonBytes(root))))
		apiError, ok := err.(*APIError)
		if result != nil || !ok || apiError.Status != 502 || apiError.Kind != "model_metadata_missing" {
			t.Fatalf("invalid capability metadata was accepted: %v", err)
		}
		if strings.Contains(err.Error(), "private-value") {
			t.Fatal("error leaked metadata values")
		}
	}
}
