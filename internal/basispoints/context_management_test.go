package basispoints

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestContextManagementWireEncoding(t *testing.T) {
	cases := []struct {
		name        string
		source      string
		wantPresent bool
	}{
		{"omitted", `{"input":"Reply OK"}`, false},
		{"null", `{"input":"Reply OK","context_management":null}`, false},
		{"empty", `{"input":"Reply OK","context_management":[]}`, false},
		{"explicit-threshold", `{"input":"Reply OK","context_management":[{"type":"compaction","compact_threshold":475000}]}`, true},
		{"server-threshold", `{"input":"Reply OK","context_management":[{"type":"compaction"}]}`, true},
		{"invalid-type-not-hidden", `{"input":"Reply OK","context_management":"invalid-policy"}`, true},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			for _, original := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/original=%t", tc.name, stream, original), func(t *testing.T) {
					service := newHTTPTestService()
					calls := 0
					service.SetHost(func(method string, payload any, out any) error {
						calls++
						expectedMethod := "host.http.do"
						if stream {
							expectedMethod = "host.http.do_stream"
						}
						if method != expectedMethod {
							t.Fatalf("method = %s", method)
						}
						encoded := payload.(map[string]any)["body"].([]byte)
						var wire, source map[string]any
						if err := json.Unmarshal(encoded, &wire); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(tc.source), &source); err != nil {
							t.Fatal(err)
						}
						policy, present := wire["context_management"]
						if present != tc.wantPresent {
							t.Fatalf("wire policy presence = %t, want %t; policy=%v", present, tc.wantPresent, policy)
						}
						if present && !reflect.DeepEqual(policy, source["context_management"]) {
							t.Fatalf("policy was changed: %v", policy)
						}
						if entries, ok := policy.([]any); present && ok && len(entries) == 0 {
							t.Fatal("sent empty context_management array")
						}
						if stream {
							*out.(*upstreamStream) = upstreamStream{StatusCode: 200, StreamID: "test-stream"}
						} else {
							*out.(*upstreamResponse) = upstreamResponse{StatusCode: 200}
						}
						return nil
					})
					request := ExecutorRequest{Model: DefaultModelID, Stream: stream, Payload: []byte(tc.source), StorageJSON: jsonBytes(map[string]any{"access_token": "test-access", "account_id": "test-account"})}
					if original {
						request.OriginalRequest = []byte(tc.source)
					}
					body, credential, err := service.prepareRequest(request)
					if err != nil {
						t.Fatal(err)
					}
					if stream {
						_, err = service.upstreamStream(request, body, credential)
					} else {
						_, err = service.upstreamRequest(request, body, credential, false)
					}
					if err != nil {
						t.Fatal(err)
					}
					if calls != 1 {
						t.Fatalf("host calls = %d, want 1", calls)
					}
				})
			}
		}
	}
}
