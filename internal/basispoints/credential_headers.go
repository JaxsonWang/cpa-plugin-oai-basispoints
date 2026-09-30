package basispoints

import (
	"net/http"
	"strings"
)

const maxCapturedHeaderValue = 32768

// The captured-header allowlist maps lowercase names to canonical output names.
// Identity headers (token, account IDs, and auth mode) come only from the current OAuth credential.
var capturedHeaderNames = map[string]string{
	"user-agent":                                          "User-Agent",
	"x-openai-account-user-id":                            "X-OpenAI-Account-User-ID",
	"x-openai-internal-basispoints-browser-name":          "X-OpenAI-Internal-Basispoints-Browser-Name",
	"x-openai-internal-basispoints-browser-ua-brands":     "X-OpenAI-Internal-Basispoints-Browser-UA-Brands",
	"x-openai-internal-basispoints-browser-ua-mobile":     "X-OpenAI-Internal-Basispoints-Browser-UA-Mobile",
	"x-openai-internal-basispoints-browser-ua-platform":   "X-OpenAI-Internal-Basispoints-Browser-UA-Platform",
	"x-openai-internal-basispoints-client-agent-profile":  "X-OpenAI-Internal-Basispoints-Client-Agent-Profile",
	"x-openai-internal-basispoints-client-editor":         "X-OpenAI-Internal-Basispoints-Client-Editor",
	"x-openai-internal-basispoints-client-host":           "X-OpenAI-Internal-Basispoints-Client-Host",
	"x-openai-internal-basispoints-client-platform":       "X-OpenAI-Internal-Basispoints-Client-Platform",
	"x-openai-internal-basispoints-client-platform-class": "X-OpenAI-Internal-Basispoints-Client-Platform-Class",
	"x-openai-internal-basispoints-client-product":        "X-OpenAI-Internal-Basispoints-Client-Product",
	"x-openai-internal-basispoints-client-runtime":        "X-OpenAI-Internal-Basispoints-Client-Runtime",
	"x-openai-internal-basispoints-office-host":           "X-OpenAI-Internal-Basispoints-Office-Host",
	"x-openai-internal-basispoints-office-platform":       "X-OpenAI-Internal-Basispoints-Office-Platform",
	"x-stainless-arch":                                    "X-Stainless-Arch",
	"x-stainless-lang":                                    "X-Stainless-Lang",
	"x-stainless-os":                                      "X-Stainless-OS",
	"x-stainless-package-version":                         "X-Stainless-Package-Version",
	"x-stainless-retry-count":                             "X-Stainless-Retry-Count",
	"x-stainless-runtime":                                 "X-Stainless-Runtime",
	"x-stainless-runtime-version":                         "X-Stainless-Runtime-Version",
}

var defaultClientHeaders = map[string]string{
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
}

func responseHeaders(c credential, stream bool) http.Header {
	headers := c.CapturedHeaders.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	for name, value := range defaultClientHeaders {
		if headerValue(headers, name) == "" {
			setHeaderExact(headers, name, value)
		}
	}
	if c.AccessToken != "" {
		setHeaderExact(headers, "Authorization", "Bearer "+c.AccessToken)
	}
	if c.AccountID != "" {
		setHeaderExact(headers, "ChatGPT-Account-ID", c.AccountID)
		setHeaderExact(headers, "X-OpenAI-Account-ID", c.AccountID)
	}
	if c.AccountUserID != "" {
		setHeaderExact(headers, "X-OpenAI-Account-User-ID", c.AccountUserID)
	}
	if c.AuthMode != "" {
		setHeaderExact(headers, "X-Basispoints-Auth-Mode", c.AuthMode)
	}
	if stream {
		setHeaderExact(headers, "Accept", "text/event-stream")
	} else {
		setHeaderExact(headers, "Accept", "application/json")
	}
	// Protocol headers are generated here and never inherited from the captured session.
	setHeaderExact(headers, "Accept-Encoding", "identity")
	setHeaderExact(headers, "Content-Type", "application/json")
	setHeaderExact(headers, "Origin", "https://bps.openai.com")
	return headers
}

func setHeaderExact(headers http.Header, name string, values ...string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
	headers[name] = append([]string(nil), values...)
}

func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func (c credential) redactMessage(message string) string {
	redacted := redactTokenMessage(message)
	// Only User-Agent carries a device fingerprint; the other captured headers are
	// client-profile constants such as excel or node and are not identity secrets.
	secrets := append([]string{c.AccessToken, c.AccountID, c.AccountUserID, c.Email}, c.CapturedHeaders.Values("User-Agent")...)
	for _, secret := range secrets {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
		}
	}
	return redacted
}
