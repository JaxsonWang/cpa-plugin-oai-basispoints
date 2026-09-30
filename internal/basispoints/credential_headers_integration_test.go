package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Build a syntactically valid fixture token whose claims intentionally differ from
// stale captured headers so the current OAuth credential takes precedence.
func headerIntegrationJWT(t *testing.T, account, user string) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":      account,
			"chatgpt_account_user_id": user,
		},
	}) + ".signature"
}

func headerIntegrationCredential(t *testing.T, token, account, user, ua string, websockets bool) string {
	t.Helper()
	return string(jsonBytes(map[string]any{
		"type":         "codex",
		"access_token": token,
		"account_id":   account,
		"userInfo": map[string]any{
			"chatgpt_account_id":      account,
			"chatgpt_account_user_id": user,
		},
		"id_token":   headerIntegrationJWT(t, account, user),
		"websockets": websockets,
		"proxy_url":  "direct",
		"headers": map[string]string{
			"Authorization":                                       "Bearer stale-captured-token",
			"ChatGPT-Account-ID":                                  "stale-captured-account",
			"X-OpenAI-Account-ID":                                 "stale-captured-account",
			"X-OpenAI-Account-User-ID":                            "captured-" + user,
			"X-OpenAI-Internal-Basispoints-Browser-Name":          "Chrome",
			"X-OpenAI-Internal-Basispoints-Browser-UA-Brands":     `"Chromium";v="140"`,
			"X-OpenAI-Internal-Basispoints-Browser-UA-Mobile":     "?0",
			"X-OpenAI-Internal-Basispoints-Browser-UA-Platform":   `"macOS"`,
			"X-OpenAI-Internal-Basispoints-Client-Agent-Profile":  "captured-profile",
			"X-OpenAI-Internal-Basispoints-Client-Editor":         "captured-editor",
			"X-OpenAI-Internal-Basispoints-Client-Host":           "captured-host",
			"X-OpenAI-Internal-Basispoints-Client-Platform":       "captured-platform",
			"X-OpenAI-Internal-Basispoints-Client-Platform-Class": "captured-class",
			"X-OpenAI-Internal-Basispoints-Client-Product":        "captured-product",
			"X-OpenAI-Internal-Basispoints-Client-Runtime":        "captured-runtime",
			"X-OpenAI-Internal-Basispoints-Office-Host":           "captured-office-host",
			"X-OpenAI-Internal-Basispoints-Office-Platform":       "captured-office-platform",
			"X-Stainless-Arch":                                    "arm64",
			"X-Stainless-Lang":                                    "typescript",
			"X-Stainless-OS":                                      "Darwin",
			"X-Stainless-Package-Version":                         "7.0.0",
			"X-Stainless-Retry-Count":                             "4",
			"X-Stainless-Runtime":                                 "node",
			"X-Stainless-Runtime-Version":                         "v22.1.0",
			"User-Agent":                                          ua,
			"Cookie":                                              "must-not-forward",
			"X-Untrusted-Header":                                  "must-not-forward",
		},
	}))
}

func headerIntegrationHost(fixture *hostCredentialFixture, capture *websocketCapture) HostCall {
	return func(method string, payload any, out any) error {
		if strings.HasPrefix(method, "host.auth.") {
			return fixture.host(method, payload, out)
		}
		return capture.host(method, payload, out)
	}
}

func headerIntegrationRequest(stream bool) ExecutorRequest {
	return ExecutorRequest{
		Model: DefaultModelID, Format: "openai-response", Stream: stream,
		StreamID: "header-integration", Payload: jsonBytes(map[string]any{
			"model": DefaultModelID, "input": "header integration",
		}),
		// These client-supplied values must not enter the trusted upstream session headers.
		Headers: http.Header{"X-OpenAI-Account-User-ID": {"attacker"}, "Cookie": {"attacker"}},
	}
}

func headerIntegrationResponse(text string) []byte {
	return jsonBytes(map[string]any{
		"id": "header-response", "status": "completed", "output": []any{
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": text},
			}},
		},
	})
}

func TestCredentialHeadersAcceptsCapturedHeadersField(t *testing.T) {
	raw := jsonBytes(map[string]any{
		"type":         "codex",
		"access_token": "captured-token",
		"account_id":   "captured-account",
		"captured_headers": map[string]string{
			"User-Agent":               "Mozilla/5.0 captured",
			"X-OpenAI-Account-User-ID": "captured-user",
			"Cookie":                   "must-not-forward",
		},
	})
	credential, err := parseCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccountUserID != "captured-user" || headerValue(credential.CapturedHeaders, "User-Agent") != "Mozilla/5.0 captured" {
		t.Fatalf("captured headers were not loaded: %+v", credential)
	}
	if headerValue(credential.CapturedHeaders, "Cookie") != "" {
		t.Fatal("captured disallowed header was retained")
	}
}

func TestCredentialHeadersAcceptsJSONHTTPHeaderValues(t *testing.T) {
	raw := jsonBytes(map[string]any{
		"type":         "codex",
		"access_token": "array-token",
		"account_id":   "array-account",
		"headers": map[string]any{
			"User-Agent":                  []string{"Mozilla/5.0 array", "Mozilla/5.0 duplicate"},
			"X-OpenAI-Account-User-ID":    []string{"array-user"},
			"X-Stainless-Runtime-Version": []string{"v22.1.0"},
			"Cookie":                      []string{"must-not-forward"},
		},
	})
	credential, err := parseCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccountUserID != "array-user" {
		t.Fatalf("array account user ID = %q", credential.AccountUserID)
	}
	if got := credential.CapturedHeaders.Values("User-Agent"); !reflect.DeepEqual(got, []string{"Mozilla/5.0 array", "Mozilla/5.0 duplicate"}) {
		t.Fatalf("array User-Agent = %#v", got)
	}
	if headerValue(credential.CapturedHeaders, "Cookie") != "" {
		t.Fatal("captured disallowed array header was retained")
	}
}

func TestCredentialHeadersRejectInvalidValuesAsUnauthorized(t *testing.T) {
	for name, value := range map[string]string{
		"oversized":        strings.Repeat("x", maxCapturedHeaderValue+1),
		"padded_oversized": strings.Repeat(" ", maxCapturedHeaderValue) + "x",
		"control":          "Mozilla/5.0\nforged: true",
		"leading_control":  "\rMozilla/5.0",
		"trailing_control": "Mozilla/5.0\n",
		"control_only":     "\t",
		"delete_control":   "Mozilla/5.0\x7f",
	} {
		for _, field := range []string{"headers", "captured_headers"} {
			for _, array := range []bool{false, true} {
				format := "string"
				var capturedValue any = value
				if array {
					format = "array"
					capturedValue = []string{"", "valid", value}
				}
				t.Run(name+"/"+field+"/"+format, func(t *testing.T) {
					raw := jsonBytes(map[string]any{
						"access_token": "invalid-header-token",
						"account_id":   "invalid-header-account",
						field:          map[string]any{"User-Agent": capturedValue},
					})
					_, err := parseCredential(raw)
					var api *APIError
					if !errors.As(err, &api) || api.Status != http.StatusUnauthorized || api.Kind != "invalid_auth" {
						t.Fatalf("invalid captured header error = %v, want 401 invalid_auth", err)
					}
				})
			}
		}
	}
}

func TestCredentialFromExecutorLoadsCapturedHeadersFromAuthAttributes(t *testing.T) {
	credential, err := credentialFromExecutor(ExecutorRequest{
		AuthMetadata: map[string]any{
			"access_token": "metadata-token",
			"account_id":   "metadata-account",
			"headers": map[string]string{
				"X-Stainless-OS": "metadata-header",
			},
			"captured_headers": map[string]string{
				"User-Agent":       "captured-user-agent",
				"X-Stainless-Arch": "captured-arch",
			},
		},
		AuthAttributes: map[string]string{
			"header:User-Agent":                  "Mozilla/5.0 executor",
			"header:X-Stainless-Runtime-Version": "v22.1.0",
			"header:X-Stainless-OS":              "attribute-os",
			"header:Cookie":                      "must-not-forward",
			"header:X-OpenAI-Account-User-ID":    "metadata-user",
			"header:ChatGPT-Account-ID":          "stale-account",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "metadata-token" || credential.AccountID != "metadata-account" {
		t.Fatalf("executor credential identity = %#v", credential)
	}
	if credential.AccountUserID != "metadata-user" || headerValue(credential.CapturedHeaders, "User-Agent") != "captured-user-agent" || headerValue(credential.CapturedHeaders, "X-Stainless-Arch") != "captured-arch" || headerValue(credential.CapturedHeaders, "X-Stainless-Runtime-Version") != "v22.1.0" || headerValue(credential.CapturedHeaders, "X-Stainless-OS") != "attribute-os" {
		t.Fatalf("executor captured headers = %#v", credential.CapturedHeaders)
	}
	if headerValue(credential.CapturedHeaders, "Cookie") != "" || headerValue(credential.CapturedHeaders, "ChatGPT-Account-ID") != "" {
		t.Fatalf("executor forwarded disallowed identity headers: %#v", credential.CapturedHeaders)
	}
	withoutAttributes, err := credentialFromExecutor(ExecutorRequest{
		AuthMetadata: map[string]any{
			"access_token": "metadata-token",
			"account_id":   "metadata-account",
			"headers": map[string]string{
				"X-Stainless-OS": "metadata-header",
			},
		},
	})
	if err != nil || headerValue(withoutAttributes.CapturedHeaders, "X-Stainless-OS") != "metadata-header" {
		t.Fatalf("executor metadata headers were not preserved without attributes: credential=%#v err=%v", withoutAttributes.CapturedHeaders, err)
	}
}

func TestHostCredentialDynamicHeadersRefreshAndClientHeaderIsolation(t *testing.T) {
	var mu sync.Mutex
	var received []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(headerIntegrationResponse("headers-ok"))
	}))
	defer server.Close()

	svc := hostModeService(t)
	svc.cfg.ResponsesURL = server.URL
	fixture := &hostCredentialFixture{entries: []hostAuthEntry{{Index: "one", Name: "one.json", Provider: AuthProviderID, Path: "/one.json"}}, files: map[string]string{
		"one": headerIntegrationCredential(t, "token-one", "account-one", "user-one", "Mozilla/5.0 one", false),
	}}
	capture := &websocketCapture{closed: make(chan struct{}, 1)}
	svc.SetHost(headerIntegrationHost(fixture, capture))
	if _, err := svc.Handle("executor.execute", jsonBytes(headerIntegrationRequest(false))); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.files["one"] = headerIntegrationCredential(t, "token-two", "account-two", "user-two", "Mozilla/5.0 two", false)
	fixture.mu.Unlock()
	if _, err := svc.Handle("executor.execute", jsonBytes(headerIntegrationRequest(false))); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("upstream requests=%d, want 2", len(received))
	}
	for i, want := range []struct{ token, account, user, ua string }{{"token-one", "account-one", "user-one", "Mozilla/5.0 one"}, {"token-two", "account-two", "user-two", "Mozilla/5.0 two"}} {
		h := received[i]
		if h.Get("Authorization") != "Bearer "+want.token || h.Get("ChatGPT-Account-ID") != want.account || h.Get("X-OpenAI-Account-ID") != want.account || h.Get("X-OpenAI-Account-User-ID") != want.user || h.Get("User-Agent") != want.ua {
			t.Fatalf("request %d trusted identity headers = %#v", i, h)
		}
		if h.Get("X-OpenAI-Internal-Basispoints-Browser-Name") != "Chrome" || h.Get("X-Stainless-Runtime-Version") != "v22.1.0" || h.Get("X-OpenAI-Internal-Basispoints-Client-Agent-Profile") != "captured-profile" {
			t.Fatalf("request %d dynamic captured headers were lost: %#v", i, h)
		}
		if h.Get("Cookie") != "" || h.Get("X-Untrusted-Header") != "" || h.Get("X-OpenAI-Account-User-ID") == "attacker" {
			t.Fatalf("request %d accepted untrusted client headers: %#v", i, h)
		}
	}
}

func TestHostCredentialDynamicHeadersFillGHCPDefaultsWithoutPluginUA(t *testing.T) {
	var received http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		_, _ = w.Write(headerIntegrationResponse("defaults-ok"))
	}))
	defer server.Close()

	raw := []byte(headerIntegrationCredential(t, "token-defaults", "account-defaults", "user-defaults", "", false))
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	delete(root, "headers")
	fixture := &hostCredentialFixture{entries: []hostAuthEntry{{Index: "defaults", Name: "defaults.json", Provider: AuthProviderID, Path: "/defaults.json"}}, files: map[string]string{
		"defaults": string(jsonBytes(root)),
	}}
	svc := hostModeService(t)
	svc.cfg.ResponsesURL = server.URL
	capture := &websocketCapture{closed: make(chan struct{}, 1)}
	svc.SetHost(headerIntegrationHost(fixture, capture))
	if _, err := svc.Handle("executor.execute", jsonBytes(headerIntegrationRequest(false))); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"X-Basispoints-Auth-Mode":                             "chatgpt",
		"X-OpenAI-Internal-Basispoints-Client-Agent-Profile":  "excel",
		"X-OpenAI-Internal-Basispoints-Client-Editor":         "excel",
		"X-OpenAI-Internal-Basispoints-Client-Host":           "office",
		"X-OpenAI-Internal-Basispoints-Client-Platform":       "excel",
		"X-OpenAI-Internal-Basispoints-Client-Platform-Class": "PC",
		"X-OpenAI-Internal-Basispoints-Client-Product":        "basispoints-excel-plugin",
		"X-OpenAI-Internal-Basispoints-Client-Runtime":        "desktop",
		"X-OpenAI-Internal-Basispoints-Office-Host":           "Excel",
		"X-OpenAI-Internal-Basispoints-Office-Platform":       "PC",
		"X-Stainless-Arch":                                    "unknown",
		"X-Stainless-Lang":                                    "js",
		"X-Stainless-OS":                                      "Unknown",
		"X-Stainless-Package-Version":                         "6.31.0",
		"X-Stainless-Retry-Count":                             "0",
		"X-Stainless-Runtime":                                 "browser:chrome",
	} {
		if received.Get(name) != want {
			t.Fatalf("default %s=%q, want %q; headers=%#v", name, received.Get(name), want, received)
		}
	}
	if strings.HasPrefix(received.Get("User-Agent"), "oai-basispoints/") {
		t.Fatalf("missing captured User-Agent fell back to plugin identity: %q", received.Get("User-Agent"))
	}
}

func TestHostCredentialDynamicHeadersHTTPStreamAndWebSocket(t *testing.T) {
	for _, mode := range []string{"sse", "ws"} {
		t.Run(mode, func(t *testing.T) {
			var seen http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Clone()
				if mode == "ws" {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					var request map[string]any
					if err := conn.ReadJSON(&request); err != nil {
						t.Error(err)
						return
					}
					_ = writeWebSocketEvents(conn, syntheticStream(incrementalTerminal("dynamic-ok")))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(syntheticStream(incrementalTerminal("dynamic-ok")))
			}))
			defer server.Close()
			svc := hostModeService(t)
			svc.cfg.ResponsesURL = server.URL
			if mode == "ws" {
				svc.cfg.UpstreamTransport = "auto"
			}
			fixture := &hostCredentialFixture{entries: []hostAuthEntry{{Index: "one", Name: "one.json", Provider: AuthProviderID, Path: "/one.json"}}, files: map[string]string{
				"one": headerIntegrationCredential(t, "token-ws", "account-ws", "user-ws", "Mozilla/5.0 ws", mode == "ws"),
			}}
			capture := &websocketCapture{closed: make(chan struct{}, 1)}
			svc.SetHost(headerIntegrationHost(fixture, capture))
			request := headerIntegrationRequest(mode == "sse")
			method := "executor.execute"
			if mode == "sse" {
				method = "executor.execute_stream"
			}
			if _, err := svc.Handle(method, jsonBytes(request)); err != nil {
				t.Fatal(err)
			}
			if mode == "sse" {
				select {
				case <-capture.closed:
				case <-time.After(3 * time.Second):
					t.Fatal("SSE stream did not close")
				}
			}
			if seen.Get("X-OpenAI-Account-User-ID") != "user-ws" || seen.Get("User-Agent") != "Mozilla/5.0 ws" || seen.Get("X-Stainless-Runtime-Version") != "v22.1.0" {
				t.Fatalf("%s headers = %#v", mode, seen)
			}
		})
	}
}

func TestHostCredential403PreservesStatusAndDoesNotSwitchCredential(t *testing.T) {
	var attempts atomic.Int32
	var account string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		account = r.Header.Get("ChatGPT-Account-ID")
		message := `{"error":{"message":"account_user_id=` + r.Header.Get("X-OpenAI-Account-User-ID") + ` user-agent=` + r.Header.Get("User-Agent") + `"}}`
		http.Error(w, message, http.StatusForbidden)
	}))
	defer server.Close()
	svc := hostModeService(t)
	svc.cfg.ResponsesURL = server.URL
	fixture := &hostCredentialFixture{entries: []hostAuthEntry{
		{Index: "a", Name: "a.json", Provider: AuthProviderID, Path: "/a.json"},
		{Index: "b", Name: "b.json", Provider: AuthProviderID, Path: "/b.json"},
	}, files: map[string]string{
		"a": headerIntegrationCredential(t, "token-a", "account-a", "user-a", "UA-a", false),
		"b": headerIntegrationCredential(t, "token-b", "account-b", "user-b", "UA-b", false),
	}}
	capture := &websocketCapture{closed: make(chan struct{}, 1)}
	svc.SetHost(headerIntegrationHost(fixture, capture))
	_, err := svc.Handle("executor.execute", jsonBytes(headerIntegrationRequest(false)))
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusForbidden {
		t.Fatalf("403 status was not preserved: %v", err)
	}
	if attempts.Load() != 1 || account != "account-a" {
		t.Fatalf("403 request switched credentials: attempts=%d account=%s", attempts.Load(), account)
	}
	if strings.Contains(api.Error(), "user-a") || strings.Contains(api.Error(), "UA-a") {
		t.Fatalf("403 diagnostics leaked dynamic identity: %s", api.Error())
	}
}

func TestHostCredentialImageRequestsUseStandardHeaders(t *testing.T) {
	var imageResponse, imageUpload, plainResponse bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Copilot-Vision-Request", "X-Untrusted-Header"} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("%s forwarded unsupported captured header %s: %q", r.URL.Path, name, got)
			}
		}
		for name, want := range map[string]string{
			"Authorization":            "Bearer token-image",
			"ChatGPT-Account-ID":       "account-image",
			"X-OpenAI-Account-User-ID": "user-image",
			"User-Agent":               "Mozilla/5.0 image",
			"Origin":                   "https://bps.openai.com",
			"Accept":                   "application/json",
			"Accept-Encoding":          "identity",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s header %s = %q, want %q", r.URL.Path, name, got, want)
			}
		}
		switch r.URL.Path {
		case "/attachments":
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=") {
				t.Errorf("attachment Content-Type = %q", r.Header.Get("Content-Type"))
			}
			imageUpload = true
			_, _ = io.WriteString(w, `{"openai_file_id":"header-image-file"}`)
		case "/responses":
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("response Content-Type = %q", got)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			isImage := strings.Contains(string(jsonBytes(body)), "header-image-file")
			if isImage {
				imageResponse = true
			} else {
				plainResponse = true
			}
			_, _ = w.Write(headerIntegrationResponse("image-ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	svc := hostModeService(t)
	svc.cfg.ResponsesURL = server.URL + "/responses"
	var root map[string]any
	if err := json.Unmarshal([]byte(headerIntegrationCredential(t, "token-image", "account-image", "user-image", "Mozilla/5.0 image", false)), &root); err != nil {
		t.Fatal(err)
	}
	root["headers"].(map[string]any)["X-Untrusted-Header"] = "true"
	root["headers"].(map[string]any)["Copilot-Vision-Request"] = "true"
	root["captured_headers"] = map[string]string{
		"X-Untrusted-Header":     "true",
		"Copilot-Vision-Request": "true",
	}
	fixture := &hostCredentialFixture{entries: []hostAuthEntry{{Index: "one", Name: "one.json", Provider: AuthProviderID, Path: "/one.json"}}, files: map[string]string{
		"one": string(jsonBytes(root)),
	}}
	capture := &websocketCapture{closed: make(chan struct{}, 1)}
	svc.SetHost(headerIntegrationHost(fixture, capture))
	plain := headerIntegrationRequest(false)
	if _, err := svc.Handle("executor.execute", jsonBytes(plain)); err != nil {
		t.Fatal(err)
	}
	dataURL, _ := testImageDataURL(t)
	imageRequest := headerIntegrationRequest(false)
	imageRequest.Payload = jsonBytes(map[string]any{"model": DefaultModelID, "input": []any{map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": dataURL}},
	}}})
	if _, err := svc.Handle("executor.execute", jsonBytes(imageRequest)); err != nil {
		t.Fatal(err)
	}
	if !plainResponse || !imageResponse || !imageUpload {
		t.Fatalf("image request handling plain=%t image-response=%t upload=%t", plainResponse, imageResponse, imageUpload)
	}
}

func TestCredentialHeadersDoNotSupplyIdentity(t *testing.T) {
	raw := jsonBytes(map[string]any{
		"type": "codex",
		"headers": map[string]string{
			"Authorization":      "Bearer stale-captured-token",
			"ChatGPT-Account-ID": "stale-captured-account",
		},
	})
	if _, err := parseCredential(raw); err == nil {
		t.Fatal("stale captured Authorization was accepted as the OAuth token")
	}
	raw = jsonBytes(map[string]any{
		"access_token": "current-token",
		"headers":      map[string]string{"ChatGPT-Account-ID": "stale-captured-account"},
	})
	if _, err := parseCredential(raw); err == nil {
		t.Fatal("stale captured ChatGPT-Account-ID was accepted as the account ID")
	}
}

func TestCredentialHeadersUseCanonicalOutputNames(t *testing.T) {
	credential, err := parseCredential(jsonBytes(map[string]any{
		"access_token": "token",
		"account_id":   "account",
		"headers": map[string]string{
			"x-stainless-os": "Darwin",
			"x-openai-internal-basispoints-browser-ua-brands": `"Chromium";v="140"`,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	headers := responseHeaders(credential, false)
	for _, name := range []string{"X-Stainless-OS", "X-OpenAI-Internal-Basispoints-Browser-UA-Brands"} {
		if _, ok := headers[name]; !ok {
			t.Fatalf("header %s missing canonical casing: %#v", name, headers)
		}
	}
	if got := headers["X-Stainless-OS"]; !reflect.DeepEqual(got, []string{"Darwin"}) {
		t.Fatalf("captured X-Stainless-OS was not kept over default: %#v", got)
	}
}

func TestCredentialRedactionKeepsClientProfileText(t *testing.T) {
	c := credential{CapturedHeaders: http.Header{
		"User-Agent":       {"Mozilla/5.0 private"},
		"X-Stainless-Arch": {"unknown"},
	}}
	message := c.redactMessage("unknown parameter from Mozilla/5.0 private")
	if message != "unknown parameter from [REDACTED]" {
		t.Fatalf("redacted message = %q", message)
	}
}
