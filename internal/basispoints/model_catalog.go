package basispoints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// catalogInterceptRequest 使用 CPA v7.3.16 已有的响应拦截 JSON 契约。
type catalogInterceptRequest struct {
	SourceFormat    string
	Model           string
	RequestedModel  string
	Stream          bool
	OriginalRequest []byte
	RequestBody     []byte
	Body            []byte
	StatusCode      int
}

// interceptModelCatalog 只修改 Codex 模型目录中由本插件配置的别名。
func (s *Service) interceptModelCatalog(raw json.RawMessage) (any, error) {
	var request catalogInterceptRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fail(400, "invalid_request", "model catalog interceptor request is invalid")
	}
	if request.SourceFormat != "openai" || request.StatusCode != http.StatusOK || request.Stream ||
		request.Model != "" || request.RequestedModel != "" || len(request.OriginalRequest) != 0 || len(request.RequestBody) != 0 {
		return map[string]any{}, nil
	}
	var catalog map[string]json.RawMessage
	if json.Unmarshal(request.Body, &catalog) != nil {
		return map[string]any{}, nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(catalog["models"], &entries) != nil || len(entries) == 0 {
		return map[string]any{}, nil
	}
	cfg := s.config()
	models := make([]map[string]json.RawMessage, len(entries))
	bySlug := make(map[string]map[string]json.RawMessage, len(entries))
	for i, entry := range entries {
		if json.Unmarshal(entry, &models[i]) != nil {
			continue
		}
		var slug string
		if json.Unmarshal(models[i]["slug"], &slug) == nil && slug != "" {
			bySlug[slug] = models[i]
		}
	}
	changed := false
	for i, model := range models {
		var slug string
		if json.Unmarshal(model["slug"], &slug) != nil {
			continue
		}
		canonicalSlug, owned := catalogCanonicalSlug(slug, cfg)
		if !owned {
			continue
		}
		canonical := bySlug[canonicalSlug]
		// 只使用同一目录的规范模型数据，不猜测容量，也不读取客户端配置。
		for _, field := range []string{"context_window", "max_context_window"} {
			var value int64
			if json.Unmarshal(canonical[field], &value) != nil || value <= 0 {
				return nil, fail(502, "model_metadata_missing", fmt.Sprintf("Basis Points model %q requires canonical model %q with a positive %s in the same Codex catalog", slug, canonicalSlug, field))
			}
			model[field] = canonical[field]
		}
		if percent, exists := canonical["effective_context_window_percent"]; exists {
			model["effective_context_window_percent"] = percent
		} else {
			delete(model, "effective_context_window_percent")
		}
		// 规范模型的 Fast 能力不能当作 Basis Points 通道的能力。
		model["service_tiers"] = jsonBytes([]any{})
		model["additional_speed_tiers"] = jsonBytes([]string{})
		updated, err := json.Marshal(model)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(entries[i], updated) {
			entries[i] = updated
			changed = true
		}
	}
	if !changed {
		return map[string]any{}, nil
	}
	catalog["models"] = jsonBytes(entries)
	body, err := json.Marshal(catalog)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Body": body}, nil
}

func catalogCanonicalSlug(slug string, cfg Config) (string, bool) {
	if upstream, ok := cfg.upstreamModelForAlias(slug); ok {
		return upstream, true
	}
	// CPA 的凭据前缀属于路由标识，仅使用同一前缀下的规范模型。
	if prefix, base, found := strings.Cut(slug, "/"); found {
		if upstream, ok := cfg.upstreamModelForAlias(base); ok {
			return prefix + "/" + upstream, true
		}
	}
	return "", false
}
