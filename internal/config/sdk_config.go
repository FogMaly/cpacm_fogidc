// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// DisableHTTP2 disables HTTP/2 for upstream requests and forces HTTP/1.1 transport.
	DisableHTTP2 bool `yaml:"disable-http2,omitempty" json:"disable-http2,omitempty"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// RequestLogMaxBodyBytes caps how many bytes of request/response payloads are retained
	// in request logs for non-streaming traffic and upstream request snapshots.
	// <= 0 falls back to the default cap.
	RequestLogMaxBodyBytes int `yaml:"request-log-max-body-bytes,omitempty" json:"request-log-max-body-bytes,omitempty"`

	// RequestLogStreamMaxBytes caps how many bytes of streaming upstream response bodies are
	// retained for request logging. <= 0 falls back to the default cap.
	RequestLogStreamMaxBytes int `yaml:"request-log-stream-max-bytes,omitempty" json:"request-log-stream-max-bytes,omitempty"`

	// UsageDetailsMemoryWindowMinutes limits how long per-request usage details stay in memory.
	// Aggregated counters remain available beyond this window. <= 0 falls back to the default window.
	UsageDetailsMemoryWindowMinutes int `yaml:"usage-details-memory-window-minutes,omitempty" json:"usage-details-memory-window-minutes,omitempty"`

	// UsageDetailsRetentionDays controls how long asynchronously persisted usage detail files
	// are retained on disk. <= 0 falls back to the default retention.
	UsageDetailsRetentionDays int `yaml:"usage-details-retention-days,omitempty" json:"usage-details-retention-days,omitempty"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
