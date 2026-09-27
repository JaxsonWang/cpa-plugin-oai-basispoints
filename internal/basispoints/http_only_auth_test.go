package basispoints

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestHTTPOnlyAuthIDsTransportIsolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ids    []string
		authID string
		mode   string
		flag   string
		wantWS bool
	}{
		{"matching-credential", []string{"bp-b"}, "bp-b", "auto", "true", false},
		{"other-credential", []string{"bp-b"}, "bp-a", "auto", "true", true},
		{"not-a-prefix", []string{"bp-b"}, "bp-b-extra", "auto", "true", true},
		{"not-a-wildcard", []string{"bp-*"}, "bp-b", "auto", "true", true},
		{"account-is-not-auth-id", []string{"fixture-account"}, "bp-b", "auto", "true", true},
		{"case-sensitive", []string{"bp-b"}, "BP-B", "auto", "true", true},
		{"missing-auth-id", []string{"bp-b"}, "", "auto", "true", true},
		{"session-is-not-auth-id", []string{"session-b"}, "bp-a", "auto", "true", true},
		{"empty-override", []string{}, "bp-b", "auto", "true", true},
		{"normalized-id", []string{" bp-b ", "bp-b"}, "bp-b", "auto", "true", false},
		{"global-http", []string{"bp-b"}, "bp-a", "http", "true", false},
		{"credential-switch-off", []string{"bp-b"}, "bp-a", "auto", "false", false},
		{"credential-switch-missing", []string{"bp-b"}, "bp-a", "auto", "", false},
		{"credential-switch-invalid", []string{"bp-b"}, "bp-a", "auto", "invalid", false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var upgrades atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upgrades.Add(1)
					if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
						t.Error("transport selection changed the selected credential")
					}
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						t.Error(err)
						return
					}
					if err := writeWebSocketEvents(conn, syntheticStream(incrementalTerminal("WS_OK"))); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				svc := NewService()
				// 通过公开 YAML 入口配置，使旧实现先暴露实际错误握手，而非仅编译失败。
				config := fmt.Sprintf("data_dir: \"\"\nresponses_url: %s\nupstream_transport: %s\nhttp_only_auth_ids: %s\n", server.URL, tc.mode, jsonBytes(tc.ids))
				if err := svc.configure(jsonBytes(map[string]any{"config_yaml": []byte(config)})); err != nil {
					t.Fatal(err)
				}
				capture := &websocketCapture{closed: make(chan struct{}, 1)}
				svc.SetHost(func(method string, payload any, out any) error {
					if method == "host.http.do" || method == "host.http.do_stream" {
						headers := payload.(map[string]any)["headers"].(http.Header)
						if headers.Get("Authorization") != "Bearer fixture" {
							t.Error("HTTP override changed the credential")
						}
					}
					return capture.host(method, payload, out)
				})
				request := websocketRequest(stream)
				request.AuthID = tc.authID
				request.AuthAttributes = map[string]string{"websockets": tc.flag}
				request.Metadata = map[string]any{"session_id": "session-b"}
				method := "executor.execute"
				if stream {
					method = "executor.execute_stream"
				}
				if _, err := svc.Handle(method, jsonBytes(request)); err != nil {
					t.Fatal(err)
				}
				svc.streamWG.Wait()
				wantWS := int32(0)
				if tc.wantWS {
					wantWS = 1
				}
				if upgrades.Load() != wantWS || capture.fallbacks.Load() != 1-wantWS {
					t.Fatalf("wrong transport: WS=%d HTTP=%d; want %d/%d", upgrades.Load(), capture.fallbacks.Load(), wantWS, 1-wantWS)
				}
			})
		}
	}
}

func TestHTTPOnlyAuthIDsConfiguration(t *testing.T) {
	cfg := defaultConfig()
	cfg.DataDir = ""
	cfg.HTTPOnlyAuthIDs = []string{" bp-b ", "bp-b", "bp-a"}
	svc := NewService()
	configureForTest(t, svc, cfg)
	want := []string{"bp-b", "bp-a"}
	if !reflect.DeepEqual(svc.config().HTTPOnlyAuthIDs, want) {
		t.Fatal("AuthIDs were not trimmed and deduplicated")
	}
	snapshot := svc.config()
	snapshot.HTTPOnlyAuthIDs[0] = "changed"
	status := svc.status()
	status["http_only_auth_ids"].([]string)[0] = "changed-status"
	if !reflect.DeepEqual(svc.config().HTTPOnlyAuthIDs, want) {
		t.Fatal("configuration snapshots share the mutable override list")
	}
	fields := registration(svc.config())["metadata"].(map[string]any)["ConfigFields"].([]map[string]any)
	found := false
	for _, field := range fields {
		if field["Name"] == "http_only_auth_ids" {
			found = field["Type"] == "array"
		}
	}
	if !found {
		t.Fatal("management configuration field is missing")
	}
	for _, input := range []string{`[""]`, `["bp-b", "  "]`, `bp-b`, `{bp-b: http}`} {
		t.Run("invalid/"+input, func(t *testing.T) {
			before := svc.config()
			config := []byte("data_dir: \"\"\nhttp_only_auth_ids: " + input + "\n")
			err := svc.configure(jsonBytes(map[string]any{"config_yaml": config}))
			if api, ok := err.(*APIError); !ok || api.Status != 400 || api.Kind != "invalid_config" {
				t.Fatalf("invalid override configuration accepted: %v", err)
			}
			if !reflect.DeepEqual(svc.config(), before) {
				t.Fatal("invalid reconfiguration replaced active settings")
			}
		})
	}
	for _, input := range []string{"[]", "null"} {
		if err := svc.configure(jsonBytes(map[string]any{"config_yaml": []byte("data_dir: \"\"\nhttp_only_auth_ids: " + input)})); err != nil {
			t.Fatal(err)
		}
		if len(svc.config().HTTPOnlyAuthIDs) != 0 {
			t.Fatal("clearing the override list retained stale IDs")
		}
	}
}

func TestHTTPOnlyAuthIDsPersistence(t *testing.T) {
	cfg := defaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.HTTPOnlyAuthIDs = []string{"bp-b"}
	svc := NewService()
	configureForTest(t, svc, cfg)
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil || !reflect.DeepEqual(saved.HTTPOnlyAuthIDs, cfg.HTTPOnlyAuthIDs) {
		t.Fatal("override list was not persisted")
	}
	restored := NewService()
	configureForTest(t, restored, cfg)
	if !reflect.DeepEqual(restored.config().HTTPOnlyAuthIDs, cfg.HTTPOnlyAuthIDs) {
		t.Fatal("override list was not restored")
	}
	// 延续已有 settings.json 覆盖 YAML 的约定，不借新字段改变其他配置优先级。
	cfg.HTTPOnlyAuthIDs = []string{"bp-a"}
	configureForTest(t, restored, cfg)
	if !reflect.DeepEqual(restored.config().HTTPOnlyAuthIDs, []string{"bp-b"}) {
		t.Fatal("existing persisted-settings precedence changed")
	}
}

func TestHTTPOnlyAuthIDsPreservesNativeCredential(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			svc := NewService()
			cfg := defaultConfig()
			cfg.DataDir = ""
			cfg.HTTPOnlyAuthIDs = []string{"bp-fixture"}
			configureForTest(t, svc, cfg)
			raw := jsonBytes(map[string]any{"type": "codex", "access_token": "fixture", "account_id": "fixture", "websockets": enabled})
			parsed, err := svc.Handle("auth.parse", jsonBytes(map[string]any{"Provider": "codex", "FileName": "fixture.json", "RawJSON": raw}))
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range parsed.(map[string]any)["Auths"].([]any) {
				auth := value.(map[string]any)
				if auth["Metadata"].(map[string]any)["websockets"] != enabled {
					t.Fatal("plugin override changed a native or virtual credential flag")
				}
				if string(auth["StorageJSON"].([]byte)) != string(raw) {
					t.Fatal("plugin override rewrote the source credential")
				}
			}
		})
	}
}

func TestHTTPOnlyAuthIDsConcurrentIsolationAndRemoval(t *testing.T) {
	var upgrades atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		if err := writeWebSocketEvents(conn, syntheticStream(incrementalTerminal("WS_OK"))); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	svc := NewService()
	cfg := defaultConfig()
	cfg.DataDir, cfg.ResponsesURL = "", server.URL
	cfg.HTTPOnlyAuthIDs = []string{"bp-b"}
	configureForTest(t, svc, cfg)
	capture := &websocketCapture{closed: make(chan struct{}, 1)}
	svc.SetHost(capture.host)
	var workers sync.WaitGroup
	for i := 0; i < 20; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			request := websocketRequest(false)
			request.AuthID = "bp-a"
			wantText := "WS_OK"
			if i%2 == 0 {
				request.AuthID, wantText = "bp-b", "HTTP_OK"
			}
			request.Metadata = map[string]any{"session_id": fmt.Sprintf("session-%d", i)}
			result, err := svc.Handle("executor.execute", jsonBytes(request))
			if err != nil {
				t.Error(err)
				return
			}
			var response map[string]any
			if err := json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &response); err != nil {
				t.Error(err)
				return
			}
			item := response["output"].([]any)[0].(map[string]any)
			if item["content"].([]any)[0].(map[string]any)["text"] != wantText {
				t.Error("one credential inherited another credential's transport")
			}
		}(i)
	}
	workers.Wait()
	svc.streamWG.Wait()
	if upgrades.Load() != 10 || capture.fallbacks.Load() != 10 {
		t.Fatalf("mixed routing failed: WS=%d HTTP=%d", upgrades.Load(), capture.fallbacks.Load())
	}
	cfg.HTTPOnlyAuthIDs = []string{}
	configureForTest(t, svc, cfg)
	request := websocketRequest(false)
	request.AuthID = "bp-b"
	if _, err := svc.Handle("executor.execute", jsonBytes(request)); err != nil {
		t.Fatal(err)
	}
	if upgrades.Load() != 11 || capture.fallbacks.Load() != 10 {
		t.Fatal("removing the override did not restore WS for the same AuthID")
	}
}
