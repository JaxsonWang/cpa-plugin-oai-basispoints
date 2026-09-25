package basispoints

import "testing"

func TestUnsupportedClaudeProtocolIsRequestScoped(t *testing.T) {
	s := NewService()
	for _, tc := range []struct {
		format, model, requested string
		reject                   bool
	}{
		{"claude", DefaultModelID, "", true},
		{"claude", "tenant/" + DefaultModelID + "(high)", "", true},
		{"claude", DefaultUpstreamModel, DefaultModelID, true},
		{"claude", DefaultUpstreamModel, "", false},
		{"claude", "claude-opus-4-6", "", false},
		{"openai-response", DefaultModelID, "", false},
	} {
		result, err := s.Handle("request.intercept_before", jsonBytes(requestInterceptRequest{SourceFormat: tc.format, Model: tc.model, RequestedModel: tc.requested}))
		if err != nil {
			t.Fatal(err)
		}
		reply := result.(map[string]any)
		if (reply["Terminate"] == true) != tc.reject {
			t.Fatalf("wrong ownership for %+v: %v", tc, reply)
		}
		if tc.reject && reply["StatusCode"] != 400 {
			t.Fatalf("wrong protocol error status: %v", reply)
		}
	}
	if registration(s.config())["capabilities"].(map[string]any)["request_interceptor"] != true {
		t.Fatal("request interceptor not registered")
	}
	result, err := s.Handle("request.intercept_after", jsonBytes(map[string]any{}))
	if err != nil || len(result.(map[string]any)) != 0 {
		t.Fatal("after-auth hook must not change other requests")
	}
}
