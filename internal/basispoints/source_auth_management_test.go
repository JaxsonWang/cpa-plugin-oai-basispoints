package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type sourceAuthFixture struct {
	mu      sync.Mutex
	entries []sourceAuthEntry
	sources map[string]sourceAuthJSON
	saved   []string
	fail    string
}

func (f *sourceAuthFixture) host(method string, payload any, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method == f.fail {
		return errors.New("sensitive fixture-token must not escape")
	}
	request := payload.(map[string]any)
	if request["host_callback_id"] != "fixture-callback" {
		return errors.New("missing callback context")
	}
	var result any
	switch method {
	case "host.auth.list":
		result = map[string]any{"files": f.entries}
	case "host.auth.get":
		source, found := f.sources[request["auth_index"].(string)]
		if !found {
			return errors.New("not found")
		}
		var err error
		source.JSON, err = os.ReadFile(source.Path)
		if err != nil {
			return err
		}
		result = source
	case "host.auth.save":
		f.saved = append(f.saved, request["name"].(string))
		return errors.New("source editor must not use the rewriting host save callback")
	default:
		return fmt.Errorf("unexpected callback %s", method)
	}
	return json.Unmarshal(jsonBytes(result), out)
}

func newSourceAuthFixture(t *testing.T) (*Service, *sourceAuthFixture) {
	t.Helper()
	svc := NewService()
	dir := t.TempDir()
	f := &sourceAuthFixture{sources: make(map[string]sourceAuthJSON)}
	for _, id := range []string{"first", "second"} {
		name := id + ".json"
		path := filepath.Join(dir, name)
		f.entries = append(f.entries, sourceAuthEntry{Index: id, Name: name, Provider: "codex", Path: path})
		f.sources[id] = sourceAuthJSON{Name: name, Path: path, JSON: json.RawMessage(`{"type":"codex","access_token":"fixture-token","refresh_token":"fixture-refresh","account_id":"fixture-account","websockets":false,"large":9007199254740993,"custom":{"preserved":true}}`)}
		if err := os.WriteFile(path, f.sources[id].JSON, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.entries = append(f.entries, sourceAuthEntry{Index: "derived", Name: "first.json", Provider: Provider, Path: filepath.Join(dir, "first.json")})
	svc.SetHost(f.host)
	if _, err := svc.Handle("auth.parse", jsonBytes(map[string]any{"Provider": "codex", "FileName": "first.json", "Path": filepath.Join(dir, "first.json"), "RawJSON": []byte(f.sources["first"].JSON), "Host": map[string]string{"AuthDir": dir}})); err != nil {
		t.Fatal(err)
	}
	return svc, f
}

func readFixtureSource(t *testing.T, fixture *sourceAuthFixture, index string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixture.sources[index].Path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sourceManagementCall(t *testing.T, svc *Service, method, path string, body []byte) (int, []byte) {
	t.Helper()
	result, err := svc.Handle("management.handle", jsonBytes(sourceAuthManagementRequest{Method: method, Path: path, Body: body, CallbackID: "fixture-callback"}))
	if err != nil {
		t.Fatal(err)
	}
	response := result.(map[string]any)
	return response["StatusCode"].(int), response["Body"].([]byte)
}

func TestSourceAuthManagementWritesOnlyExistingWebsocketField(t *testing.T) {
	svc, fixture := newSourceAuthFixture(t)
	original := bytes.Clone(fixture.sources["first"].JSON)
	for _, enabled := range []bool{true, false} {
		status, body := sourceManagementCall(t, svc, http.MethodPatch, sourceAuthAPI, jsonBytes(map[string]any{"auth_index": "first", "websockets": enabled}))
		if status != 200 || !bytes.Contains(body, []byte(`"saved":true`)) {
			t.Fatalf("save failed: %d %s", status, body)
		}
		var before, after map[string]json.RawMessage
		_ = json.Unmarshal(original, &before)
		_ = json.Unmarshal(readFixtureSource(t, fixture, "first"), &after)
		if !bytes.Equal(after["websockets"], jsonBytes(enabled)) {
			t.Fatal("websocket flag did not persist")
		}
		delete(before, "websockets")
		delete(after, "websockets")
		if !bytes.Equal(jsonBytes(before), jsonBytes(after)) {
			t.Fatal("unrelated source fields changed")
		}
		parsed, err := authParse(jsonBytes(map[string]any{"Provider": "codex", "FileName": "first.json", "RawJSON": readFixtureSource(t, fixture, "first")}))
		if err != nil {
			t.Fatal(err)
		}
		derived := parsed["Auths"].([]any)[1].(map[string]any)
		request := ExecutorRequest{AuthAttributes: derived["Attributes"].(map[string]string), AuthMetadata: derived["Metadata"].(map[string]any)}
		if credentialWebsocketsEnabled(request) != enabled || svc.config().UpstreamTransport != "auto" {
			t.Fatal("saved source did not feed the unchanged auto credential gate")
		}
	}
	if !bytes.Equal(readFixtureSource(t, fixture, "second"), original) || len(fixture.saved) != 0 {
		t.Fatal("another source was changed")
	}
	beforeInfo, _ := os.Stat(fixture.sources["first"].Path)
	_, _ = sourceManagementCall(t, svc, http.MethodPatch, sourceAuthAPI, []byte(`{"auth_index":"first","websockets":false}`))
	afterInfo, _ := os.Stat(fixture.sources["first"].Path)
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("unchanged boolean should not rewrite the source")
	}
}

func TestSourceAuthManagementReadsLatestSourceAndNeverReturnsTokens(t *testing.T) {
	svc, fixture := newSourceAuthFixture(t)
	status, response := sourceManagementCall(t, svc, "GET", sourceAuthAPI, nil)
	if status != 200 || bytes.Contains(response, []byte("token")) || bytes.Contains(response, []byte(fixture.sources["first"].Path)) || bytes.Count(response, []byte(`"auth_index"`)) != 2 {
		t.Fatal("list leaked source data or included derived records")
	}
	source := fixture.sources["first"]
	source.JSON = bytes.ReplaceAll(source.JSON, []byte("fixture-token"), []byte("newly-refreshed-token"))
	fixture.sources["first"] = source
	if err := os.WriteFile(source.Path, source.JSON, 0600); err != nil {
		t.Fatal(err)
	}
	status, response = sourceManagementCall(t, svc, "PATCH", sourceAuthAPI, []byte(`{"auth_index":"first","websockets":true}`))
	if status != 200 || bytes.Contains(response, []byte("token")) || !bytes.Contains(readFixtureSource(t, fixture, "first"), []byte("newly-refreshed-token")) {
		t.Fatal("save used a stale source snapshot or leaked a token")
	}
}

func TestSourceAuthManagementRejectsInvalidAndDerivedTargets(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `{"auth_index":"first"}`, `{"auth_index":"first","websockets":null}`,
		`{"auth_index":"first","websockets":"true"}`, `{"auth_index":"first","websockets":1}`,
		`{"auth_index":"first","websockets":true,"access_token":"replacement"}`,
		`{"auth_index":"first","websockets":true} {}`, `{"auth_index":"derived","websockets":true}`,
		`{"auth_index":"../first.json","websockets":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			svc, fixture := newSourceAuthFixture(t)
			status, _ := sourceManagementCall(t, svc, "PATCH", sourceAuthAPI, []byte(body))
			if status < 400 || len(fixture.saved) != 0 {
				t.Fatal("invalid target or patch was accepted")
			}
		})
	}
}

func TestSourceAuthManagementRejectsChangedAndNestedSources(t *testing.T) {
	for _, mode := range []string{"name", "path", "provider", "invalid-json", "null", "nested"} {
		t.Run(mode, func(t *testing.T) {
			svc, fixture := newSourceAuthFixture(t)
			source := fixture.sources["first"]
			switch mode {
			case "name":
				fixture.entries[0].Name = "../first.json"
			case "path":
				fixture.entries[0].Path = filepath.Join(filepath.Dir(source.Path), "second.json")
			case "provider":
				source.JSON = []byte(`{"type":"meta"}`)
			case "invalid-json":
				source.JSON = []byte(`{`)
			case "null":
				source.JSON = []byte(`null`)
			case "nested":
				fixture.entries[0].Path = filepath.Join(filepath.Dir(source.Path), "nested", "first.json")
			}
			fixture.sources["first"] = source
			if err := os.WriteFile(source.Path, source.JSON, 0600); err != nil {
				t.Fatal(err)
			}
			status, _ := sourceManagementCall(t, svc, "PATCH", sourceAuthAPI, []byte(`{"auth_index":"first","websockets":true}`))
			if status < 400 || len(fixture.saved) != 0 {
				t.Fatal("source location/type check failed")
			}
		})
	}
}

func TestSourceAuthManagementPropagatesHostFailureWithoutSecretDetails(t *testing.T) {
	for _, method := range []string{"host.auth.list", "host.auth.get"} {
		t.Run(method, func(t *testing.T) {
			svc, fixture := newSourceAuthFixture(t)
			fixture.fail = method
			status, body := sourceManagementCall(t, svc, "GET", sourceAuthAPI, nil)
			if status != 502 || bytes.Contains(body, []byte("fixture-token")) || bytes.Contains(body, []byte(`"saved":true`)) || len(fixture.saved) != 0 {
				t.Fatal("host failure was hidden or sensitive details escaped")
			}
		})
	}
}

func TestSourceAuthManagementPublicPageContainsNoCredentialData(t *testing.T) {
	svc, fixture := newSourceAuthFixture(t)
	registration, err := svc.Handle("management.register", []byte(`{"ResourceBasePath":"/v0/resource/plugins/fixture-plugin"}`))
	if err != nil || len(registration.(map[string]any)["routes"].([]map[string]string)) != 2 {
		t.Fatal("management registration failed")
	}
	fixture.fail = "host.auth.list"
	page := "/v0/resource/plugins/fixture-plugin/source-auths"
	status, body := sourceManagementCall(t, svc, "GET", page, nil)
	if status != 200 || bytes.Contains(body, []byte("fixture-token")) || !bytes.Contains(body, []byte("upstream_transport: auto")) {
		t.Fatal("static page touched credentials or omitted transport semantics")
	}
	for _, bad := range []string{"sessionStorage", ".innerHTML", "parent.", "postMessage"} {
		if strings.Contains(string(body), bad) {
			t.Fatalf("unsafe credential-page mechanism: %s", bad)
		}
	}
	status, _ = sourceManagementCall(t, svc, "PATCH", page, []byte(`{"auth_index":"first","websockets":true}`))
	if status != 404 || len(fixture.saved) != 0 {
		t.Fatal("public resource accepted a mutation")
	}
}

func TestSourceAuthManagementConcurrentDifferentSources(t *testing.T) {
	svc, fixture := newSourceAuthFixture(t)
	var wg sync.WaitGroup
	for _, index := range []string{"first", "second"} {
		wg.Go(func() {
			status, _ := sourceManagementCall(t, svc, "PATCH", sourceAuthAPI, jsonBytes(map[string]any{"auth_index": index, "websockets": true}))
			if status != 200 {
				t.Error("concurrent save failed")
			}
		})
	}
	wg.Wait()
	for _, index := range []string{"first", "second"} {
		if !credentialWebsocketsEnabled(ExecutorRequest{StorageJSON: readFixtureSource(t, fixture, index)}) {
			t.Fatal("a concurrent update was lost")
		}
	}
}

func TestSourceAuthManagementRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	svc, fixture := newSourceAuthFixture(t)
	path := fixture.sources["first"].Path
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fixture.sources["second"].Path, path); err != nil {
		t.Fatal(err)
	}
	before := readFixtureSource(t, fixture, "second")
	status, _ := sourceManagementCall(t, svc, "PATCH", sourceAuthAPI, []byte(`{"auth_index":"first","websockets":true}`))
	if status != 409 || !bytes.Equal(before, readFixtureSource(t, fixture, "second")) {
		t.Fatal("symlink target was modified")
	}
}

func TestSourceAuthAtomicWriteFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing-directory.json")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeSourceAuthFields(target, []byte(`{"websockets":true}`)); err == nil {
		t.Fatal("expected rename failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "existing-directory.json" {
		t.Fatal("temporary file was not cleaned")
	}
}
