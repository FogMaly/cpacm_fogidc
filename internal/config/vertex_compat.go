package config

import "strings"

// VertexCompatKey represents the configuration for Vertex AI-compatible API keys.
// This supports third-party services that use Vertex AI-style endpoint paths
// (/publishers/google/models/{model}:streamGenerateContent) but authenticate
// with simple API keys instead of Google Cloud service account credentials.
//
// Example services: zenmux.ai and similar Vertex-compatible providers.
type VertexCompatKey struct {
	// APIKey is the authentication key for accessing the Vertex-compatible API.
	// Maps to the x-goog-api-key header.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Name is an optional display name for management UI and auth labels.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces model aliases for this credential (e.g., "teamA/vertex-pro").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Vertex-compatible API endpoint.
	// The executor will append "/v1/publishers/google/models/{model}:action" to this.
	// Example: "https://zenmux.ai/api" becomes "https://zenmux.ai/api/v1/publishers/google/models/..."
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	// Commonly used for cookies, user-agent, and other authentication headers.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	Groups                                []string `yaml:"groups,omitempty" json:"groups,omitempty"`
	Tag                                   string   `yaml:"tag,omitempty" json:"tag,omitempty"`
	Remark                                string   `yaml:"remark,omitempty" json:"remark,omitempty"`
	Weight                                int      `yaml:"weight,omitempty" json:"weight,omitempty"`
	AutoBan                               int      `yaml:"auto_ban,omitempty" json:"auto_ban,omitempty"`
	ParamOverride                         string   `yaml:"param_override,omitempty" json:"param_override,omitempty"`
	HeaderOverride                        string   `yaml:"header_override,omitempty" json:"header_override,omitempty"`
	StatusCodeMapping                     string   `yaml:"status_code_mapping,omitempty" json:"status_code_mapping,omitempty"`
	AllowServiceTier                      bool     `yaml:"allow_service_tier,omitempty" json:"allow_service_tier,omitempty"`
	DisableStore                          bool     `yaml:"disable_store,omitempty" json:"disable_store,omitempty"`
	AllowSafetyIdentifier                 bool     `yaml:"allow_safety_identifier,omitempty" json:"allow_safety_identifier,omitempty"`
	AllowIncludeObfuscation               bool     `yaml:"allow_include_obfuscation,omitempty" json:"allow_include_obfuscation,omitempty"`
	UpstreamModelUpdateCheckEnabled       bool     `yaml:"upstream_model_update_check_enabled,omitempty" json:"upstream_model_update_check_enabled,omitempty"`
	UpstreamModelUpdateAutoSyncEnabled    bool     `yaml:"upstream_model_update_auto_sync_enabled,omitempty" json:"upstream_model_update_auto_sync_enabled,omitempty"`
	UpstreamModelUpdateLastCheckTime      int64    `yaml:"upstream_model_update_last_check_time,omitempty" json:"upstream_model_update_last_check_time,omitempty"`
	UpstreamModelUpdateLastDetectedModels []string `yaml:"upstream_model_update_last_detected_models,omitempty" json:"upstream_model_update_last_detected_models,omitempty"`
	UpstreamModelUpdateIgnoredModels      string   `yaml:"upstream_model_update_ignored_models,omitempty" json:"upstream_model_update_ignored_models,omitempty"`

	// Models defines the model configurations including aliases for routing.
	Models []VertexCompatModel `yaml:"models,omitempty" json:"models,omitempty"`
}

func (k VertexCompatKey) GetAPIKey() string  { return k.APIKey }
func (k VertexCompatKey) GetBaseURL() string { return k.BaseURL }

// VertexCompatModel represents a model configuration for Vertex compatibility,
// including the actual model name and its alias for API routing.
type VertexCompatModel struct {
	// Name is the actual model name used by the external provider.
	Name string `yaml:"name" json:"name"`

	// Alias is the model name alias that clients will use to reference this model.
	Alias string `yaml:"alias" json:"alias"`
}

func (m VertexCompatModel) GetName() string  { return m.Name }
func (m VertexCompatModel) GetAlias() string { return m.Alias }

// SanitizeVertexCompatKeys deduplicates and normalizes Vertex-compatible API key credentials.
func (cfg *Config) SanitizeVertexCompatKeys() {
	if cfg == nil {
		return
	}

	seen := make(map[string]struct{}, len(cfg.VertexCompatAPIKey))
	out := cfg.VertexCompatAPIKey[:0]
	for i := range cfg.VertexCompatAPIKey {
		entry := cfg.VertexCompatAPIKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		if entry.BaseURL == "" {
			// BaseURL is required for Vertex API key entries
			continue
		}
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)

		// Sanitize models: remove entries without valid alias
		sanitizedModels := make([]VertexCompatModel, 0, len(entry.Models))
		for _, model := range entry.Models {
			model.Alias = strings.TrimSpace(model.Alias)
			model.Name = strings.TrimSpace(model.Name)
			if model.Alias != "" && model.Name != "" {
				sanitizedModels = append(sanitizedModels, model)
			}
		}
		entry.Models = sanitizedModels

		// Use API key + base URL as uniqueness key
		uniqueKey := entry.APIKey + "|" + entry.BaseURL
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	cfg.VertexCompatAPIKey = out
}
