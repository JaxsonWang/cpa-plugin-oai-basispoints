package basispoints

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestEncryptedAgentMessageRejectedBeforeHost(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, sourceFormat := range []string{"openai-response", "codex"} {
			t.Run(sourceFormat+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				svc := NewService()
				calls := 0
				svc.SetHost(func(string, any, any) error { calls++; return nil })
				source := map[string]any{
					"model": DefaultModelID,
					"input": []any{map[string]any{
						"type": "agent_message", "author": "/root", "recipient": "/root/child",
						"content": []any{
							map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK"},
							map[string]any{"type": "encrypted_content", "encrypted_content": "private-message-content"},
						},
					}},
				}
				request := ExecutorRequest{Model: DefaultModelID, Format: sourceFormat, SourceFormat: sourceFormat, Stream: stream, Payload: jsonBytes(source), AuthMetadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}, StreamID: "fixture-stream"}
				if sourceFormat == "codex" {
					request.OriginalRequest = jsonBytes(map[string]any{"model": "ignored-original"})
				}
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				_, err := svc.Handle(method, jsonBytes(request))
				var apiError *APIError
				if !errors.As(err, &apiError) || apiError.Status != 400 || apiError.Kind != "unsupported_encrypted_agent_message" || calls != 0 {
					t.Fatalf("error=%v host_calls=%d", err, calls)
				}
				if strings.Contains(err.Error(), "private-message") {
					t.Fatal("error leaked agent message content")
				}
			})
		}
	}
}

func TestAgentMessageValidationPreservesPlaintextAndReasoning(t *testing.T) {
	plain := map[string]any{"type": "agent_message", "author": "/root", "recipient": "/root/child", "content": []any{map[string]any{"type": "input_text", "text": "Read-only task\n中文  text"}}}
	reasoning := map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-reasoning"}
	source := map[string]any{"model": DefaultModelID, "input": []any{plain, reasoning}}
	before := string(jsonBytes(source))
	body, err := prepareResponsesBody(source, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items := body["input"].([]any)
	if !reflect.DeepEqual(items[len(items)-2], plain) || !reflect.DeepEqual(items[len(items)-1], reasoning) {
		t.Fatal("modified plaintext agent message or opaque reasoning")
	}
	if string(jsonBytes(source)) != before {
		t.Fatal("mutated caller input")
	}
}

func TestCatalogDoesNotAdvertiseEncryptedMultiAgentProtocol(t *testing.T) {
	root, models := decodedCatalog(t, catalogFixture(DefaultUpstreamModel, DefaultModelID))
	for _, model := range models[:2] {
		model["multi_agent_version"] = jsonBytes("v2")
		model["multi_agent_reasoning_effort"] = jsonBytes("xhigh")
	}
	root["models"] = jsonBytes(models)
	result, err := NewService().Handle("response.intercept_after", jsonBytes(catalogRequest(jsonBytes(root))))
	if err != nil {
		t.Fatal(err)
	}
	_, updated := decodedCatalog(t, result.(map[string]any)["Body"].([]byte))
	for _, field := range []string{"multi_agent_version", "multi_agent_reasoning_effort"} {
		if _, exists := updated[1][field]; exists {
			t.Fatalf("advertised unimplemented encrypted protocol: %s", field)
		}
	}
	if !reflect.DeepEqual(updated[0], models[0]) || !reflect.DeepEqual(updated[2], models[2]) {
		t.Fatal("changed another provider's metadata")
	}
}
