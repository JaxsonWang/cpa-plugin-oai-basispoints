package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type authParseRequest struct {
	Provider string `json:"Provider"`
	Path     string `json:"Path"`
	FileName string `json:"FileName"`
	RawJSON  []byte `json:"RawJSON"`
	Host     struct {
		ProxyURL string `json:"ProxyURL"`
	} `json:"Host"`
}

type authRefreshRequest struct {
	AuthID      string            `json:"AuthID"`
	StorageJSON []byte            `json:"StorageJSON"`
	Metadata    map[string]any    `json:"Metadata"`
	Attributes  map[string]string `json:"Attributes"`
	Host        *struct {
		ProxyURL string `json:"ProxyURL"`
	} `json:"Host"`
}

type credential struct {
	AccessToken     string
	AccountID       string
	AccountUserID   string
	AuthMode        string
	Email           string
	CapturedHeaders http.Header
	ExpiresAt       time.Time
}

func parseCredential(raw []byte) (credential, error) {
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil || root == nil {
		return credential{}, fail(400, "invalid_auth", "OAuth credential is not valid JSON")
	}
	token := findToken(root)
	if token == "" {
		return credential{}, fail(401, "invalid_auth", "ChatGPT OAuth credential has no access_token")
	}
	claims := jwtPayload(token)
	capturedHeaders, err := capturedHeadersFromCredential(root)
	if err != nil {
		return credential{}, err
	}
	accountID := accountIDFromClaims(claims)
	if accountID == "" {
		accountID = findAccountID(root)
	}
	if accountID == "" {
		return credential{}, fail(401, "invalid_auth", "ChatGPT OAuth credential has no account ID")
	}
	accountUserID := accountUserIDFromClaims(claims)
	if accountUserID == "" {
		accountUserID = findAccountUserID(root)
	}
	if accountUserID == "" {
		accountUserID = headerValue(capturedHeaders, "X-OpenAI-Account-User-ID")
	}
	authMode := firstString(root, "auth_mode", "authMode")
	if !strings.EqualFold(authMode, "chatgpt") {
		authMode = "chatgpt"
	}
	email := firstString(root, "email")
	if email == "" {
		email = stringValue(claims["email"])
	}
	expiresAt := jwtExpiry(claims)
	if rawExpiry := firstValue(root, "expires_at", "expired"); expiresAt.IsZero() {
		expiresAt = timeFromValue(rawExpiry)
	}
	return credential{
		AccessToken:     token,
		AccountID:       accountID,
		AccountUserID:   accountUserID,
		AuthMode:        authMode,
		Email:           email,
		CapturedHeaders: capturedHeaders,
		ExpiresAt:       expiresAt,
	}, nil
}

func findToken(root map[string]any) string {
	for _, key := range []string{"access_token", "accessToken"} {
		if token := stringValue(root[key]); token != "" {
			return strings.TrimPrefix(strings.TrimSpace(token), "Bearer ")
		}
	}
	for _, key := range []string{"token_data", "tokenData", "sessionInfo", "session_info", "oauth", "tokens"} {
		if nested, ok := root[key].(map[string]any); ok {
			if token := findToken(nested); token != "" {
				return token
			}
		}
	}
	return ""
}

func findAccountID(root map[string]any) string {
	for _, key := range []string{"userInfo", "user_info", "auth", "token_data", "tokenData", "sessionInfo", "session_info"} {
		if nested, ok := root[key].(map[string]any); ok {
			if id := firstString(nested, "chatgpt_account_id", "account_id", "accountId"); id != "" {
				return id
			}
		}
	}
	return firstString(root, "chatgpt_account_id", "account_id", "accountId")
}

func findAccountUserID(root map[string]any) string {
	for _, key := range []string{"userInfo", "user_info", "auth", "token_data", "tokenData", "sessionInfo", "session_info"} {
		if nested, ok := root[key].(map[string]any); ok {
			if id := firstString(nested, "chatgpt_account_user_id", "account_user_id", "accountUserId"); id != "" {
				return id
			}
		}
	}
	return firstString(root, "chatgpt_account_user_id", "account_user_id", "accountUserId")
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(object[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value
		}
	}
	return nil
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

func accountIDFromClaims(claims map[string]any) string {
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if accountID := firstString(auth, "chatgpt_account_id", "account_id"); accountID != "" {
			return accountID
		}
	}
	return firstString(claims, "chatgpt_account_id", "account_id")
}

func accountUserIDFromClaims(claims map[string]any) string {
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if accountUserID := firstString(auth, "chatgpt_account_user_id", "account_user_id"); accountUserID != "" {
			return accountUserID
		}
	}
	return firstString(claims, "https://api.openai.com/auth.chatgpt_account_user_id", "chatgpt_account_user_id", "account_user_id")
}

func capturedHeadersFromCredential(root map[string]any) (http.Header, error) {
	headers, err := capturedHeadersFromValue(root["headers"])
	if err != nil {
		return nil, err
	}
	captured, err := capturedHeadersFromValue(root["captured_headers"])
	if err != nil {
		return nil, err
	}
	for name, values := range captured {
		setHeaderExact(headers, name, values...)
	}
	return headers, nil
}

func capturedHeadersFromValue(value any) (http.Header, error) {
	headers := make(http.Header)
	object, ok := value.(map[string]any)
	if !ok {
		return headers, nil
	}
	for rawName, rawValue := range object {
		name, allowed := capturedHeaderNames[strings.ToLower(strings.TrimSpace(rawName))]
		if !allowed {
			continue
		}
		values, ok := headerValues(rawValue)
		if !ok {
			continue
		}
		validated := make([]string, 0, len(values))
		for _, value := range values {
			if len(value) > maxCapturedHeaderValue || hasHeaderControl(value) {
				return nil, fail(401, "invalid_auth", "OAuth credential contains an invalid captured header")
			}
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			validated = append(validated, value)
		}
		if len(validated) == 0 {
			continue
		}
		// JSON-encoded http.Header values use string arrays; preserve validated non-empty values in order.
		setHeaderExact(headers, name, validated...)
	}
	return headers, nil
}

func headerValues(value any) ([]string, bool) {
	switch value := value.(type) {
	case string:
		return []string{value}, true
	case []any:
		values := make([]string, 0, len(value))
		for _, item := range value {
			item, ok := item.(string)
			if !ok {
				return nil, false
			}
			values = append(values, item)
		}
		return values, true
	default:
		return nil, false
	}
}

func hasHeaderControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func jwtExpiry(claims map[string]any) time.Time {
	if claims == nil {
		return time.Time{}
	}
	if value, ok := claims["exp"]; ok {
		return timeFromValue(value)
	}
	return time.Time{}
}

func timeFromValue(value any) time.Time {
	switch number := value.(type) {
	case json.Number:
		if seconds, err := number.Int64(); err == nil && seconds > 0 {
			return time.Unix(seconds, 0)
		}
	case float64:
		if number > 0 {
			return time.Unix(int64(number), 0)
		}
	case int64:
		if number > 0 {
			return time.Unix(number, 0)
		}
	case string:
		if timestamp, err := time.Parse(time.RFC3339, strings.TrimSpace(number)); err == nil {
			return timestamp
		}
	}
	return time.Time{}
}

func credentialID(fileName string) string {
	base := strings.TrimSpace(filepath.Base(fileName))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "chatgpt"
	}
	var builder strings.Builder
	builder.WriteString("bp-")
	for _, character := range base {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	return builder.String()
}

func authData(raw []byte, fileName string, c credential) map[string]any {
	websockets := credentialWebsocketsEnabled(ExecutorRequest{StorageJSON: raw})
	id := credentialID(fileName)
	label := c.Email
	if label == "" {
		label = fileName
	}
	return map[string]any{
		"Provider": Provider,
		"ID":       id,
		// 查询名称及其 basename 均不同于源文件，避免串模型和误判源文件编辑入口。
		"FileName":    Provider + "/" + id,
		"Label":       label,
		"StorageJSON": raw,
		"Metadata": map[string]any{
			"websockets": websockets,
			"type":       Provider,
			"auth_kind":  "oauth",
			"account_id": c.AccountID,
			"auth_mode":  c.AuthMode,
		},
		"Attributes": map[string]string{
			"websockets": strconv.FormatBool(websockets),
			"auth_kind":  "oauth",
			"account_id": c.AccountID,
			"auth_mode":  c.AuthMode,
		},
	}
}

func nativeCodexAuthData(raw []byte, fileName string, c credential) (map[string]any, error) {
	// 在 Basis Points 虚拟记录旁保留原生 Codex 记录，使既有 Codex 模型
	// 继续走 CPA 原生执行器，同时为 oai-basispoints 模型提供独立认证。
	label := c.Email
	if label == "" {
		label = fileName
	}
	planType := codexPlanType(raw, c.AccessToken)
	// CPA 原生执行器直接读取 Metadata，必须保留源凭据字段。
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fail(400, "invalid_auth", "OAuth credential is not valid JSON")
	}
	metadata["type"] = AuthProviderID
	metadata["auth_kind"] = "oauth"
	metadata["access_token"] = c.AccessToken
	if firstString(metadata, "account_id") == "" {
		metadata["account_id"] = c.AccountID
	}
	attributes := map[string]string{
		"auth_kind":  "oauth",
		"account_id": c.AccountID,
		"auth_mode":  c.AuthMode,
	}
	if planType != "" {
		metadata["plan_type"] = planType
		attributes["plan_type"] = planType
	}
	if priority, ok := metadata["priority"].(float64); ok {
		attributes["priority"] = strconv.Itoa(int(priority))
	} else if priority := strings.TrimSpace(stringValue(metadata["priority"])); priority != "" {
		if _, err := strconv.Atoi(priority); err == nil {
			attributes["priority"] = priority
		}
	}
	if note := strings.TrimSpace(stringValue(metadata["note"])); note != "" {
		attributes["note"] = note
	}
	return map[string]any{
		"Provider":    AuthProviderID,
		"ID":          fileName,
		"FileName":    fileName,
		"Label":       label,
		"StorageJSON": raw,
		"Metadata":    metadata,
		"Attributes":  attributes,
	}, nil
}

func codexPlanType(raw []byte, accessToken string) string {
	var root map[string]any
	if json.Unmarshal(raw, &root) == nil {
		if planType := firstString(root, "plan_type", "planType"); planType != "" {
			return planType
		}
	}
	// 与 CPA 原生解析一致，优先读取 id_token 套餐。
	idClaims := jwtPayload(stringValue(root["id_token"]))
	if auth, ok := idClaims["https://api.openai.com/auth"].(map[string]any); ok {
		if planType := firstString(auth, "chatgpt_plan_type", "plan_type"); planType != "" {
			return planType
		}
	}
	claims := jwtPayload(accessToken)
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		return firstString(auth, "chatgpt_plan_type", "plan_type")
	}
	return ""
}

func authParse(raw []byte) (map[string]any, error) {
	var request authParseRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	return parseAuthRequest(request)
}

func parseAuthRequest(request authParseRequest) (map[string]any, error) {
	provider := strings.ToLower(strings.TrimSpace(request.Provider))
	if provider != "" && provider != "codex" && provider != Provider && provider != "openai" {
		return map[string]any{"Handled": false}, nil
	}
	fileName := strings.TrimSpace(request.FileName)
	if fileName == "" {
		fileName = filepath.Base(strings.TrimSpace(request.Path))
	}
	c, err := parseCredential(request.RawJSON)
	if err != nil {
		if provider == Provider {
			return nil, err
		}
		return map[string]any{"Handled": false}, nil
	}
	virtual := authData(request.RawJSON, fileName, c)
	setWebSocketProxy(virtual, request.RawJSON, request.Host.ProxyURL)
	if provider == Provider {
		return map[string]any{"Handled": true, "Auth": virtual}, nil
	}
	native, err := nativeCodexAuthData(request.RawJSON, fileName, c)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"Handled": true,
		"Auths": []any{
			native,
			virtual,
		},
	}, nil
}

func authRefresh(raw []byte) (map[string]any, error) {
	var request authRefreshRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	c, err := parseCredential(request.StorageJSON)
	if err != nil {
		return nil, err
	}
	if !c.ExpiresAt.IsZero() && !time.Now().Before(c.ExpiresAt) {
		return nil, fail(401, "auth_expired", "ChatGPT OAuth access token has expired")
	}
	next := time.Now().Add(10 * time.Minute)
	if !c.ExpiresAt.IsZero() {
		next = c.ExpiresAt.Add(-2 * time.Minute)
		if next.Before(time.Now().Add(time.Minute)) {
			next = time.Now().Add(time.Minute)
		}
	}
	auth := authData(request.StorageJSON, "", c)
	// 刷新契约会保留未返回的身份字段，不能把 bp-* 路由 ID 当成源文件名重新生成身份。
	delete(auth, "ID")
	delete(auth, "FileName")
	delete(auth, "Label")
	websockets := credentialWebsocketsEnabled(ExecutorRequest{StorageJSON: request.StorageJSON, AuthMetadata: request.Metadata, AuthAttributes: request.Attributes})
	auth["Metadata"].(map[string]any)["websockets"] = websockets
	auth["Attributes"].(map[string]string)["websockets"] = strconv.FormatBool(websockets)
	proxyURL := request.Attributes["basispoints_proxy_url"]
	if request.Host != nil {
		proxyURL = request.Host.ProxyURL
	}
	setWebSocketProxy(auth, request.StorageJSON, proxyURL)
	return map[string]any{
		"Auth":             auth,
		"NextRefreshAfter": next.UTC(),
	}, nil
}

// WS 不经过宿主 HTTP 客户端，解析和刷新都必须继承同一份代理，不能悄悄改为直连。
func setWebSocketProxy(auth map[string]any, raw []byte, hostProxy string) {
	var settings map[string]any
	_ = json.Unmarshal(raw, &settings)
	proxyURL := strings.TrimSpace(stringValue(settings["proxy_url"]))
	if proxyURL != "" {
		auth["ProxyURL"] = proxyURL
	} else {
		proxyURL = strings.TrimSpace(hostProxy)
	}
	auth["Attributes"].(map[string]string)["basispoints_proxy_url"] = proxyURL
}

func credentialFromExecutor(request ExecutorRequest) (credential, error) {
	if len(request.StorageJSON) > 0 {
		return parseCredential(request.StorageJSON)
	}
	metadata := map[string]any{}
	for key, value := range request.AuthMetadata {
		metadata[key] = value
	}
	for key, value := range request.AuthAttributes {
		metadata[key] = value
	}
	if token := stringValue(metadata["access_token"]); token != "" {
		headers := map[string]any{}
		for key, value := range request.AuthAttributes {
			if strings.HasPrefix(strings.ToLower(key), "header:") {
				name := strings.TrimSpace(key[len("header:"):])
				if name != "" {
					headers[name] = value
				}
			}
		}
		if len(headers) > 0 {
			// CPA synchronizes header:* attributes from the credential's headers field;
			// use that runtime view while preserving captured_headers precedence.
			metadata["headers"] = headers
		}
		return parseCredential(jsonBytes(metadata))
	}
	return credential{}, fail(401, "missing_auth", "CPA did not provide a ChatGPT OAuth credential")
}

func redactTokenMessage(message string) string {
	for _, prefix := range []string{"Bearer ", "bearer "} {
		if index := strings.Index(message, prefix); index >= 0 {
			end := index + len(prefix)
			for end < len(message) && message[end] != ' ' && message[end] != '"' && message[end] != '\n' {
				end++
			}
			message = message[:index] + prefix + "[REDACTED]" + message[end:]
		}
	}
	return fmt.Sprintf("%s", message)
}
