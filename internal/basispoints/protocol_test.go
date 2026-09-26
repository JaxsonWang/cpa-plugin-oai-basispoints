package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareResponsesBodyStripsToolsAndUsesNaturalLanguageCatalog(t *testing.T) {
	cfg := defaultConfig()
	source := map[string]any{
		"model": "gpt-6-astra-basispoints",
		"input": []any{map[string]any{"role": "user", "content": "What is the weather?"}},
		"tools": []any{map[string]any{
			"type":        "function",
			"name":        "get_weather",
			"description": "Get current weather.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
		"reasoning": map[string]any{"effort": "max"},
	}
	body, err := prepareResponsesBody(source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := body["tools"]; exists {
		t.Fatal("upstream body still contains tools")
	}
	if got := body["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", got)
	}
	items, ok := body["input"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("input = %#v, want developer prologue and user history", body["input"])
	}
	first := objectValue(items[0])
	if first["role"] != "developer" {
		t.Fatalf("first input role = %v, want developer", first["role"])
	}
	content, _ := first["content"].([]any)
	catalog := itemText(objectValue(content[0])["text"])
	if !strings.Contains(catalog, "get_weather") || !strings.Contains(catalog, "city (required)") {
		t.Fatalf("catalog omitted natural language tool directory: %s", catalog)
	}
	if !strings.Contains(catalog, `"properties"`) {
		t.Fatalf("catalog omitted the complete input schema: %s", catalog)
	}
	metadata := objectValue(body["metadata"])
	if stringValue(metadata["turn_id"]) == "" || stringValue(metadata["task_id"]) == "" || metadata["agent_iteration"] != "1" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

func TestTransportUsesReferencesAndPreservesNativeItem(t *testing.T) {
	source := map[string]any{
		"tools": []any{map[string]any{
			"type": "function",
			"name": "get_weather",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
	}
	native := map[string]any{
		"type":       "function_call",
		"id":         "fc_native_weather",
		"call_id":    "call_native_weather",
		"name":       "run_officejs",
		"status":     "completed",
		"summary":    "Get current weather for Tokyo",
		"references": []any{"Tokyo weather"},
		"arguments": string(jsonBytes(map[string]any{
			"summary": "Get current weather for Tokyo",
			"code":    string(jsonBytes(map[string]any{"city": "Tokyo"})), "references": []any{"get_weather"},
		})),
	}
	response := map[string]any{"output": []any{native}}
	call, callErr := extractNativeClientToolCall(native, clientToolSpecs(source))
	if callErr != nil {
		t.Fatal("transport call was not decoded")
	}
	if call["name"] != "get_weather" || call["call_id"] != "call_native_weather" {
		t.Fatalf("decoded call = %#v", call)
	}
	if got := stringValue(call["arguments"]); got != `{"city":"Tokyo"}` {
		t.Fatalf("arguments = %s", got)
	}
	translated, _, _, err := transformResponseBody(jsonBytes(response), source)
	if err != nil {
		t.Fatal(err)
	}
	var clientResponse map[string]any
	_ = json.Unmarshal(translated, &clientResponse)
	clientCall := objectValue(clientResponse["output"].([]any)[0])
	if clientCall["name"] != "get_weather" {
		t.Fatalf("client call = %#v", clientCall)
	}
	items := translateInputItems([]any{
		clientCall,
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_native_weather",
			"output":  "18°C",
		},
	}, clientToolSpecs(source))
	if !reflect.DeepEqual(items[0], native) {
		t.Fatalf("replayed native item = %#v, want %#v", items[0], native)
	}
	output := objectValue(items[1])
	if output["type"] != "function_call_output" || output["id"] != "fc_call_native_weather" || output["output"] != "18°C" {
		t.Fatalf("normalized output = %#v", output)
	}
}

func TestTurnIDStaysStableWhileAgentIterationAdvances(t *testing.T) {
	base := map[string]any{
		"model": "gpt-6-astra-basispoints",
		"input": []any{map[string]any{"role": "user", "content": "Inspect the workbook"}},
	}
	cfg := defaultConfig()
	first, err := prepareResponsesBody(base, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nextSource := cloneObject(base)
	nextSource["input"] = append(nextSource["input"].([]any),
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "run_officejs", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
	)
	second, err := prepareResponsesBody(nextSource, cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstMetadata := objectValue(first["metadata"])
	secondMetadata := objectValue(second["metadata"])
	if firstMetadata["turn_id"] != secondMetadata["turn_id"] {
		t.Fatalf("turn_id changed: %v -> %v", firstMetadata["turn_id"], secondMetadata["turn_id"])
	}
	if secondMetadata["agent_iteration"] != "2" {
		t.Fatalf("agent_iteration = %v, want 2", secondMetadata["agent_iteration"])
	}
}

func TestParseCredentialPrefersJWTAuthAccountID(t *testing.T) {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"exp":                         4102444800,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-from-jwt"},
	}) + "."
	credential, err := parseCredential([]byte(`{"access_token":"` + token + `","account_id":"acct-from-file"}`))
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccountID != "acct-from-jwt" {
		t.Fatalf("account ID = %s, want acct-from-jwt", credential.AccountID)
	}
}

func TestAuthParseExpandsCodexFileIntoNativeAndBasisPointsAuths(t *testing.T) {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-auth-parse",
			"chatgpt_plan_type":  "plus",
		},
	}) + "."
	raw := jsonBytes(map[string]any{
		"type":         "codex",
		"access_token": token,
		"account_id":   "file-account-id",
	})
	request, _ := json.Marshal(authParseRequest{Provider: AuthProviderID, FileName: "codex-account.json", RawJSON: raw})
	response, err := authParse(request)
	if err != nil {
		t.Fatal(err)
	}
	if response["Handled"] != true {
		t.Fatalf("response = %#v, want handled", response)
	}
	auths, ok := response["Auths"].([]any)
	if !ok || len(auths) != 2 {
		t.Fatalf("Auths = %#v, want native and Basis Points records", response["Auths"])
	}
	native := objectValue(auths[0])
	if native["Provider"] != AuthProviderID || native["ID"] != "codex-account.json" {
		t.Fatalf("native auth = %#v", native)
	}
	attributes, ok := native["Attributes"].(map[string]string)
	if !ok || attributes["plan_type"] != "plus" {
		t.Fatalf("native attributes = %#v, want preserved plan type", native["Attributes"])
	}
	virtual := objectValue(auths[1])
	if virtual["Provider"] != Provider || virtual["ID"] != "bp-codex-account" {
		t.Fatalf("Basis Points auth = %#v", virtual)
	}
	metadata := objectValue(virtual["Metadata"])
	if metadata["account_id"] != "acct-auth-parse" {
		t.Fatalf("virtual metadata = %#v, want JWT account ID", metadata)
	}
}

func TestServiceUsesCodexAuthParserAndBasisPointsExecutorIdentifiers(t *testing.T) {
	service := NewService()
	authIdentifier, err := service.Handle("auth.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	if objectValue(authIdentifier)["identifier"] != AuthProviderID {
		t.Fatalf("auth identifier = %#v, want %q", authIdentifier, AuthProviderID)
	}
	executorIdentifier, err := service.Handle("executor.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	if objectValue(executorIdentifier)["identifier"] != Provider {
		t.Fatalf("executor identifier = %#v, want %q", executorIdentifier, Provider)
	}
}

func TestExecuteBuildsAuthenticatedBasisPointsBody(t *testing.T) {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"exp":                         4102444800,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-exec"},
	}) + "."
	requestBody := map[string]any{
		"model": "gpt-6-astra-basispoints",
		"input": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools": []any{map[string]any{"type": "function", "name": "demo", "parameters": map[string]any{"type": "object"}}},
	}
	rawRequest := jsonBytes(requestBody)
	storage := jsonBytes(map[string]any{"type": "codex", "access_token": token, "account_id": "wrong-file-id"})
	service := NewService()
	var seenHeaders map[string][]string
	var seenBody map[string]any
	service.SetHost(func(method string, payload any, out any) error {
		if method != "host.http.do" {
			return nil
		}
		request := objectValue(payload.(map[string]any))
		if headers, ok := request["headers"].(http.Header); ok {
			seenHeaders = map[string][]string(headers)
		}
		var body map[string]any
		if err := json.Unmarshal(request["body"].([]byte), &body); err != nil {
			return err
		}
		seenBody = body
		response := out.(*upstreamResponse)
		response.StatusCode = 200
		response.Headers = http.Header{"Content-Type": {"application/json"}}
		response.Body = jsonBytes(map[string]any{
			"id":     "resp_test",
			"status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
		})
		return nil
	})

	raw, _ := json.Marshal(map[string]any{"OriginalRequest": rawRequest, "Payload": rawRequest, "StorageJSON": storage, "Model": "gpt-6-astra-basispoints"})
	result, err := service.Handle("executor.execute", raw)
	if err != nil {
		t.Fatal(err)
	}
	if seenHeaders["Authorization"] == nil || !strings.HasPrefix(seenHeaders["Authorization"][0], "Bearer ") {
		t.Fatalf("authorization header missing: %#v", seenHeaders)
	}
	if seenHeaders["ChatGPT-Account-ID"][0] != "acct-exec" || seenHeaders["X-OpenAI-Account-ID"][0] != "acct-exec" {
		t.Fatalf("account headers = %#v", seenHeaders)
	}
	if seenHeaders["X-Basispoints-Auth-Mode"][0] != "chatgpt" {
		t.Fatalf("auth mode header = %#v", seenHeaders)
	}
	if _, exists := seenBody["tools"]; exists {
		t.Fatalf("upstream body still has tools: %#v", seenBody)
	}
	if result == nil {
		t.Fatal("executor result is nil")
	}
}
