package basispoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type hostCredentialFixture struct {
	mu      sync.Mutex
	entries []hostAuthEntry
	files   map[string]string
	gets    []string
}

func (f *hostCredentialFixture) host(method string, payload any, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	request := payload.(map[string]any)
	var result any
	switch method {
	case "host.auth.list":
		result = map[string]any{"files": f.entries}
	case "host.auth.get":
		index := request["auth_index"].(string)
		f.gets = append(f.gets, index)
		raw, found := f.files[index]
		if !found {
			return errors.New("not found")
		}
		result = map[string]any{"auth_index": index, "json": json.RawMessage(raw)}
	default:
		return fmt.Errorf("unexpected callback %s", method)
	}
	return json.Unmarshal(jsonBytes(result), out)
}

func hostModeService(t *testing.T) *Service {
	t.Helper()
	svc := NewService()
	if _, err := svc.Handle("plugin.register", jsonBytes(map[string]any{"config_yaml": []byte("data_dir: \"\"\nupstream_transport: http\n")})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Handle("model.static", jsonBytes(map[string]any{"Host": map[string]any{"ProxyURL": ""}})); err != nil {
		t.Fatal(err)
	}
	return svc
}

func codexFile(token string, expires time.Time) string {
	return string(jsonBytes(map[string]any{
		"type": "codex", "access_token": token, "refresh_token": "fixture-refresh",
		"account_id": "fixture-account", "expired": expires.UTC().Format(time.RFC3339),
	}))
}

func TestHostCredentialModeLeavesCodexFilesToHost(t *testing.T) {
	svc := hostModeService(t)
	raw := []byte(codexFile("fixture-token", time.Now().Add(time.Hour)))
	result, err := svc.Handle("auth.parse", jsonBytes(authParseRequest{Provider: AuthProviderID, FileName: "codex.json", RawJSON: raw}))
	if err != nil {
		t.Fatal(err)
	}
	// 未接管解析时，宿主按原生 codex 类型加载，不产生会跳过持久化的 plugin_virtual 认证。
	if objectValue(result)["Handled"] != false {
		t.Fatalf("host mode must not expand plugin auths: %#v", result)
	}
	capabilities := objectValue(registration(svc.config())["capabilities"])
	if capabilities["auth_provider"] != false || capabilities["model_router"] != true {
		t.Fatalf("host mode capabilities = %#v", capabilities)
	}
	virtual := objectValue(registration(newVirtualTestService().config())["capabilities"])
	if virtual["auth_provider"] != true || virtual["model_router"] != false {
		t.Fatalf("explicit virtual mode must keep the auth provider: %#v", virtual)
	}
}

func TestHostCredentialModeRoutesOnlyConfiguredAliases(t *testing.T) {
	svc := hostModeService(t)
	for model, handled := range map[string]bool{DefaultModelID: true, DefaultUpstreamModel: false, "unknown": false} {
		result, err := svc.Handle("model.route", jsonBytes(map[string]any{"RequestedModel": model}))
		if err != nil {
			t.Fatal(err)
		}
		route := objectValue(result)
		if route["Handled"] != handled || (handled && route["TargetKind"] != "self") {
			t.Fatalf("route %s = %#v", model, route)
		}
	}
	result, err := newVirtualTestService().Handle("model.route", jsonBytes(map[string]any{"RequestedModel": DefaultModelID}))
	if err != nil || objectValue(result)["Handled"] != false {
		t.Fatalf("explicit virtual mode must not route: %#v %v", result, err)
	}
}

func TestHostCredentialModeReadsCurrentHostCredential(t *testing.T) {
	var authorization []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorization = append(authorization, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer server.Close()

	svc := hostModeService(t)
	svc.cfg.ResponsesURL = server.URL
	fixture := &hostCredentialFixture{
		entries: []hostAuthEntry{
			{Index: "native", Name: "native.json", Provider: AuthProviderID, Path: "/auths/native.json"},
			{Index: "runtime", Name: "runtime", Provider: AuthProviderID, Path: "/auths/runtime.json", Runtime: true},
			{Index: "disabled", Name: "disabled.json", Provider: AuthProviderID, Path: "/auths/disabled.json", Disabled: true},
			{Index: "other", Name: "other.json", Provider: "claude", Path: "/auths/other.json"},
		},
		files: map[string]string{"native": codexFile("token-before-refresh", time.Now().Add(time.Hour))},
	}
	svc.SetHost(fixture.host)
	execute := func() {
		t.Helper()
		_, err := svc.Handle("executor.execute", jsonBytes(ExecutorRequest{
			Model: DefaultModelID, Format: "openai-response", SourceFormat: "openai-response",
			Payload: []byte(`{"model":"` + DefaultModelID + `","input":"hello"}`),
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	execute()
	// 宿主刷新并持久化后，下一次请求必须读到新 token，而不是解析时的快照。
	fixture.mu.Lock()
	fixture.files["native"] = codexFile("token-after-refresh", time.Now().Add(time.Hour))
	fixture.mu.Unlock()
	execute()
	if len(authorization) != 2 || authorization[0] != "Bearer token-before-refresh" || authorization[1] != "Bearer token-after-refresh" {
		t.Fatalf("upstream authorization = %v", authorization)
	}
	for _, index := range fixture.gets {
		if index != "native" {
			t.Fatalf("read unavailable or foreign credential %q", index)
		}
	}
}

func TestHostCredentialModeSkipsExpiredAndReportsMissing(t *testing.T) {
	svc := hostModeService(t)
	fixture := &hostCredentialFixture{
		entries: []hostAuthEntry{
			{Index: "expired", Name: "expired.json", Provider: AuthProviderID, Path: "/auths/expired.json"},
			{Index: "valid", Name: "valid.json", Provider: AuthProviderID, Path: "/auths/valid.json"},
		},
		files: map[string]string{
			"expired": codexFile("expired-token", time.Now().Add(-time.Minute)),
			"valid":   codexFile("valid-token", time.Now().Add(time.Hour)),
		},
	}
	svc.SetHost(fixture.host)
	for range 2 {
		selected, err := svc.withHostCredential(ExecutorRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if c, _ := parseCredential(selected.StorageJSON); c.AccessToken != "valid-token" || selected.AuthID != "valid" {
			t.Fatalf("selected %q with token %q", selected.AuthID, c.AccessToken)
		}
	}
	fixture.entries = nil
	_, err := svc.withHostCredential(ExecutorRequest{})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.Status != 503 || apiError.Kind != "auth_unavailable" {
		t.Fatalf("missing credential error = %v", err)
	}
}

func TestCredentialSourceValidation(t *testing.T) {
	if cfg := NewService().config(); cfg.CredentialSource != CredentialSourceHost {
		t.Fatalf("new service credential source = %q, want host", cfg.CredentialSource)
	}
	for value, want := range map[string]string{"": "host", " ": "host", "virtual": "virtual", " VIRTUAL ": "virtual", "host": "host", " HOST ": "host", "file": ""} {
		cfg := defaultConfig()
		cfg.CredentialSource = value
		if err := cfg.normalize(); (err == nil) != (want != "") {
			t.Fatalf("credential_source %q validation = %v", value, err)
		}
		if want != "" && cfg.CredentialSource != want {
			t.Fatalf("credential_source %q normalized = %q, want %q", value, cfg.CredentialSource, want)
		}
	}
}

func TestCredentialSourceConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, persisted, want string
	}{
		{name: "omitted", want: "host"},
		{name: "empty", yaml: "credential_source: ''\n", want: "host"},
		{name: "null", yaml: "credential_source: null\n", want: "host"},
		{name: "explicit_host", yaml: "credential_source: host\n", want: "host"},
		{name: "explicit_virtual", yaml: "credential_source: virtual\n", want: "virtual"},
		{name: "older_settings_without_source", persisted: `{}`, want: "host"},
		{name: "empty_persisted_source", persisted: `{"credential_source":""}`, want: "host"},
		{name: "persisted_virtual", persisted: `{"credential_source":"virtual"}`, want: "virtual"},
		{name: "persisted_virtual_over_yaml_host", yaml: "credential_source: host\n", persisted: `{"credential_source":"virtual"}`, want: "virtual"},
		{name: "persisted_host_over_yaml_virtual", yaml: "credential_source: virtual\n", persisted: `{"credential_source":"host"}`, want: "host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.persisted != "" {
				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.persisted), 0600); err != nil {
					t.Fatal(err)
				}
			}
			svc := NewService()
			raw := jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: %q\n%s", dir, tc.yaml))})
			result, err := svc.Handle("plugin.register", raw)
			if err != nil {
				t.Fatal(err)
			}
			caps := objectValue(objectValue(result)["capabilities"])
			if svc.config().CredentialSource != tc.want || caps["model_router"] != (tc.want == "host") || caps["auth_provider"] != (tc.want == "virtual") {
				t.Fatalf("credential source = %q, capabilities = %v, want %s", svc.config().CredentialSource, caps, tc.want)
			}
			restored := NewService()
			if err := restored.configure(jsonBytes(map[string]any{"config_yaml": []byte(fmt.Sprintf("data_dir: %q\n", dir))})); err != nil {
				t.Fatal(err)
			}
			if restored.config().CredentialSource != tc.want {
				t.Fatalf("restored credential source = %q, want %s", restored.config().CredentialSource, tc.want)
			}
			if _, err := svc.Handle("plugin.reconfigure", jsonBytes(map[string]any{"config_yaml": []byte("data_dir: ''\n")})); err != nil {
				t.Fatal(err)
			}
			if svc.config().CredentialSource != "host" {
				t.Fatal("reconfigure without an override must restore the host default")
			}
		})
	}
}

func TestHostCredentialRotationSkipsInvalidWithoutBias(t *testing.T) {
	svc := hostModeService(t)
	fixture := &hostCredentialFixture{
		entries: []hostAuthEntry{
			{Index: "expired", Name: "0-expired.json", Provider: AuthProviderID, Path: "/fixture/0-expired.json"},
			{Index: "a", Name: "a.json", Provider: AuthProviderID, Path: "/fixture/a.json"},
			{Index: "b", Name: "b.json", Provider: AuthProviderID, Path: "/fixture/b.json"},
		},
		files: map[string]string{
			"expired": codexFile("expired", time.Now().Add(-time.Hour)),
			"a":       codexFile("token-a", time.Now().Add(time.Hour)),
			"b":       codexFile("token-b", time.Now().Add(time.Hour)),
		},
	}
	svc.SetHost(fixture.host)
	counts := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 60 {
		wg.Go(func() {
			selected, err := svc.withHostCredential(ExecutorRequest{})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			counts[selected.AuthID]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if counts["a"] != 30 || counts["b"] != 30 {
		t.Fatalf("usable credentials were not balanced: %v", counts)
	}
}

func TestHostCredentialExhaustionKeepsDeterministicCause(t *testing.T) {
	svc := hostModeService(t)
	fixture := &hostCredentialFixture{
		entries: []hostAuthEntry{
			{Index: "expired", Name: "a.json", Provider: AuthProviderID, Path: "/fixture/a.json"},
			{Index: "missing", Name: "b.json", Provider: AuthProviderID, Path: "/fixture/b.json"},
		},
		files: map[string]string{"expired": codexFile("expired", time.Now().Add(-time.Hour))},
	}
	svc.SetHost(fixture.host)
	for _, last := range []string{"", "expired", "missing"} {
		svc.lastCredential = last
		_, err := svc.withHostCredential(ExecutorRequest{})
		var api *APIError
		if !errors.As(err, &api) || api.Status != 401 || api.Kind != "auth_expired" {
			t.Fatalf("credential error changed with cursor %q: %v", last, err)
		}
	}
}

func TestHostCredentialUsesCurrentFileSettings(t *testing.T) {
	svc := hostModeService(t)
	fixture := &hostCredentialFixture{
		entries: []hostAuthEntry{{Index: "a", Provider: AuthProviderID, Path: "/fixture/a.json"}},
		files:   map[string]string{},
	}
	svc.SetHost(fixture.host)
	for _, enabled := range []bool{true, false} {
		fixture.files["a"] = string(jsonBytes(map[string]any{
			"type": "codex", "access_token": "fixture", "account_id": "fixture-account", "websockets": enabled,
		}))
		selected, err := svc.withHostCredential(ExecutorRequest{AuthAttributes: map[string]string{"websockets": fmt.Sprint(!enabled)}})
		if err != nil || credentialWebsocketsEnabled(selected) != enabled {
			t.Fatalf("current WS setting not applied: enabled=%t err=%v", enabled, err)
		}
	}
	for _, data := range []map[string]any{
		{"type": "codex", "access_token": "fixture", "account_id": "fixture-account", "disabled": true},
		{"type": "claude", "access_token": "fixture", "account_id": "fixture-account"},
	} {
		fixture.files["a"] = string(jsonBytes(data))
		if _, err := svc.withHostCredential(ExecutorRequest{}); err == nil {
			t.Fatal("credential changed after listing was accepted")
		}
	}
}

func TestHostModeRequiresStaticHostPolicyAndRemovesManagement(t *testing.T) {
	svc := NewService()
	svc.cfg.CredentialSource = CredentialSourceHost
	if _, err := svc.withHostCredential(ExecutorRequest{}); err == nil {
		t.Fatal("missing host transport policy silently assumed direct networking")
	}
	for _, mode := range []string{CredentialSourceHost, CredentialSourceVirtual} {
		cfg := defaultConfig()
		cfg.CredentialSource = mode
		if objectValue(registration(cfg)["capabilities"])["management_api"] == true {
			t.Fatalf("management page still registered in %s mode", mode)
		}
	}
	for _, method := range []string{"management.register", "management.handle"} {
		if _, err := svc.Handle(method, []byte(`{}`)); err == nil {
			t.Fatalf("removed management method %s still handled", method)
		}
	}
}
