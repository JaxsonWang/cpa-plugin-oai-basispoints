package basispoints

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestWebSocketRequiresEnabledCredential(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attributes map[string]string
		metadata   map[string]any
		stored     map[string]any
		wantWS     bool
	}{
		{name: "missing"},
		{name: "metadata-off", metadata: map[string]any{"websockets": false}},
		{name: "metadata-on", metadata: map[string]any{"websockets": true}, wantWS: true},
		{name: "metadata-string-on", metadata: map[string]any{"websockets": " true "}, wantWS: true},
		{name: "metadata-invalid", metadata: map[string]any{"websockets": "unknown"}},
		{name: "metadata-number", metadata: map[string]any{"websockets": 1}},
		{name: "stored-on", stored: map[string]any{"websockets": true}, wantWS: true},
		{name: "stored-off", stored: map[string]any{"websockets": false}},
		{name: "attributes-off", attributes: map[string]string{"websockets": "false"}, metadata: map[string]any{"websockets": true}, stored: map[string]any{"websockets": true}},
		{name: "attributes-on", attributes: map[string]string{"websockets": "true"}, metadata: map[string]any{"websockets": false}, wantWS: true},
		{name: "updated-metadata-off", metadata: map[string]any{"websockets": false}, stored: map[string]any{"websockets": true}},
		{name: "updated-metadata-null", metadata: map[string]any{"websockets": nil}, stored: map[string]any{"websockets": true}},
	} {
		for _, mode := range []string{"auto", "http"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, mode, stream), func(t *testing.T) {
					var upgrades atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						upgrades.Add(1)
						http.Error(w, "fixture upgrade unsupported", http.StatusNotFound)
					}))
					defer server.Close()
					svc := newVirtualTestService()
					svc.cfg.ResponsesURL, svc.cfg.UpstreamTransport = server.URL, mode
					capture := &websocketCapture{closed: make(chan struct{}, 1)}
					svc.SetHost(capture.host)
					request := websocketRequest(stream)
					request.AuthAttributes = tc.attributes
					for key, value := range tc.metadata {
						request.AuthMetadata[key] = value
					}
					if tc.stored != nil {
						stored := map[string]any{"access_token": "fixture", "account_id": "fixture-account"}
						for key, value := range tc.stored {
							stored[key] = value
						}
						request.StorageJSON = jsonBytes(stored)
					}
					method := "executor.execute"
					if stream {
						method = "executor.execute_stream"
					}
					if _, err := svc.Handle(method, jsonBytes(request)); err != nil {
						t.Fatal(err)
					}
					svc.streamWG.Wait()
					want := int32(0)
					if tc.wantWS && mode == "auto" {
						want = 1
					}
					if upgrades.Load() != want || capture.fallbacks.Load() != 1 {
						t.Fatalf("credential gate failed: handshakes=%d want=%d HTTP=%d", upgrades.Load(), want, capture.fallbacks.Load())
					}
				})
			}
		}
	}
}

func TestWebSocketFlagSurvivesAuthParseAndRefresh(t *testing.T) {
	for _, value := range []any{nil, false, true, "true", "false", "invalid"} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			raw := jsonBytes(map[string]any{"type": "codex", "access_token": "fixture", "account_id": "fixture", "websockets": value})
			parsed, err := authParse(jsonBytes(map[string]any{"Provider": "codex", "FileName": "fixture.json", "RawJSON": raw}))
			if err != nil {
				t.Fatal(err)
			}
			virtual := parsed["Auths"].([]any)[1].(map[string]any)
			refreshed, err := authRefresh(jsonBytes(map[string]any{"AuthID": "fixture", "StorageJSON": raw}))
			if err != nil {
				t.Fatal(err)
			}
			want := value == true || value == "true"
			for _, auth := range []map[string]any{virtual, refreshed["Auth"].(map[string]any)} {
				if auth["Metadata"].(map[string]any)["websockets"] != want || auth["Attributes"].(map[string]string)["websockets"] != fmt.Sprint(want) {
					t.Fatal("virtual credential lost its explicit websocket setting")
				}
			}
		})
	}
}

func TestWebSocketRefreshUsesCurrentFlagOverStoredSnapshot(t *testing.T) {
	for _, source := range []string{"Attributes", "Metadata"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", source, enabled), func(t *testing.T) {
				request := map[string]any{
					"AuthID":      "fixture",
					"StorageJSON": jsonBytes(map[string]any{"access_token": "fixture", "account_id": "fixture", "websockets": !enabled}),
				}
				if source == "Attributes" {
					request[source] = map[string]string{"websockets": fmt.Sprint(enabled)}
				} else {
					request[source] = map[string]any{"websockets": enabled}
				}
				refreshed, err := authRefresh(jsonBytes(request))
				if err != nil {
					t.Fatal(err)
				}
				auth := refreshed["Auth"].(map[string]any)
				if auth["Metadata"].(map[string]any)["websockets"] != enabled || auth["Attributes"].(map[string]string)["websockets"] != fmt.Sprint(enabled) {
					t.Fatal("refresh replaced the current websocket flag with a stale snapshot")
				}
			})
		}
	}
}
