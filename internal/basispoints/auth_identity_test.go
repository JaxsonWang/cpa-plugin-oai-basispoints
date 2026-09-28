package basispoints

import (
	"bytes"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAuthParseSeparatesSourceAndVirtualModelLookupNames(t *testing.T) {
	names := make(map[string]string)
	for _, fileName := range []string{"account-a.json", "account-b.json"} {
		raw := jsonBytes(map[string]any{
			"type": "codex", "access_token": "fixture-token", "account_id": fileName,
			"refresh_token": "fixture-refresh", "websockets": true,
		})
		request := authParseRequest{Provider: AuthProviderID, FileName: fileName, RawJSON: raw}
		response, err := parseAuthRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		auths := response["Auths"].([]any)
		native, virtual := objectValue(auths[0]), objectValue(auths[1])
		if native["FileName"] != fileName || native["ID"] != fileName || native["Provider"] != AuthProviderID {
			t.Fatal("native source identity changed")
		}
		if virtual["ID"] != credentialID(fileName) || virtual["Provider"] != Provider {
			t.Fatal("virtual routing identity changed")
		}
		if got := virtual["FileName"]; got != Provider+"/"+credentialID(fileName) {
			t.Fatalf("virtual name = %v, want a provider-qualified runtime name", got)
		}
		if filepath.Base(virtual["FileName"].(string)) == fileName {
			t.Fatal("virtual name must not be treated as the editable source filename")
		}
		for _, item := range auths {
			auth := objectValue(item)
			name, provider := auth["FileName"].(string), auth["Provider"].(string)
			if previous, exists := names[name]; exists {
				t.Fatalf("model lookup name %q shared by %s and %s", name, previous, provider)
			}
			names[name] = provider
			if !bytes.Equal(auth["StorageJSON"].([]byte), raw) {
				t.Fatal("source OAuth payload changed")
			}
		}
		// 重新解析同一源文件时，查询名称和路由身份都必须稳定。
		reloaded, err := parseAuthRequest(request)
		if err != nil || !reflect.DeepEqual(reloaded, response) {
			t.Fatal("auth identities changed after source reload")
		}
	}
}

func TestAuthParseVirtualNameUsesSourcePathWhenFileNameMissing(t *testing.T) {
	response, err := parseAuthRequest(authParseRequest{
		Provider: AuthProviderID,
		Path:     filepath.Join(t.TempDir(), "source.json"),
		RawJSON:  []byte(`{"access_token":"fixture-token","account_id":"fixture-account"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	auths := response["Auths"].([]any)
	if objectValue(auths[0])["FileName"] != "source.json" || objectValue(auths[1])["FileName"] != Provider+"/bp-source" {
		t.Fatal("path-based auth names are not separated")
	}
}

func TestAuthRefreshLeavesExistingIdentityToHost(t *testing.T) {
	raw := []byte(`{"access_token":"fixture-token","account_id":"fixture-account","websockets":true}`)
	response, err := authRefresh(jsonBytes(authRefreshRequest{
		AuthID: "bp-account-a", StorageJSON: raw,
		Attributes: map[string]string{"websockets": "false"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	auth := objectValue(response["Auth"])
	// CPA 的刷新契约在字段省略时保留现有身份，不能从 AuthID 反推源文件名。
	for _, field := range []string{"ID", "FileName", "Label"} {
		if _, exists := auth[field]; exists {
			t.Fatalf("refresh must preserve the host's existing %s", field)
		}
	}
	if auth["Provider"] != Provider || !bytes.Equal(auth["StorageJSON"].([]byte), raw) {
		t.Fatal("refresh changed provider or source storage")
	}
	if objectValue(auth["Metadata"])["websockets"] != false {
		t.Fatal("refresh ignored current websocket setting")
	}
}
