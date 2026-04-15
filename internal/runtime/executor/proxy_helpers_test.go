package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestApplyTransportDefaults_SetsResponseHeaderTimeout(t *testing.T) {
	tr := applyTransportDefaults(&http.Transport{}, false)
	if tr.ResponseHeaderTimeout != defaultUpstreamResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %s, want %s", tr.ResponseHeaderTimeout, defaultUpstreamResponseHeaderTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = false, want true")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Fatalf("TLSHandshakeTimeout = %s, want > 0", tr.TLSHandshakeTimeout)
	}
	if tr.IdleConnTimeout < 90*time.Second {
		t.Fatalf("IdleConnTimeout = %s, want >= 90s", tr.IdleConnTimeout)
	}
}

func TestBuildProxyTransport_InheritsTransportDefaults(t *testing.T) {
	tr := buildProxyTransport("https://proxy.example.com:8443", false)
	if tr == nil {
		t.Fatal("buildProxyTransport returned nil")
	}
	if tr.Proxy == nil {
		t.Fatal("Proxy = nil, want configured proxy function")
	}
	if tr.ResponseHeaderTimeout != defaultUpstreamResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %s, want %s", tr.ResponseHeaderTimeout, defaultUpstreamResponseHeaderTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = false, want true")
	}
}

func TestNewIPv4PreferredTransport_DisablesEnvironmentProxy(t *testing.T) {
	tr := newIPv4PreferredTransport(false)
	if tr == nil {
		t.Fatal("newIPv4PreferredTransport returned nil")
	}
	if tr.Proxy != nil {
		t.Fatal("Proxy != nil, want nil to ensure direct connection")
	}
}

func TestBuildProxyTransport_SOCKS5DisablesEnvironmentProxy(t *testing.T) {
	tr := buildProxyTransport("socks5://user:pass@127.0.0.1:1080", false)
	if tr == nil {
		t.Fatal("buildProxyTransport returned nil")
	}
	if tr.Proxy != nil {
		t.Fatal("Proxy != nil, want nil for SOCKS5 transport")
	}
}

func TestBuildProxyTransport_HTTPProxyOverridesEnvironmentProxy(t *testing.T) {
	tr := buildProxyTransport("http://proxy.example.com:8080", false)
	if tr == nil {
		t.Fatal("buildProxyTransport returned nil")
	}
	if tr.Proxy == nil {
		t.Fatal("Proxy = nil, want configured proxy function")
	}
	req := &http.Request{URL: mustParseURL(t, "https://api.example.com/v1/chat/completions")}
	proxyURL, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy returned error: %v", err)
	}
	if proxyURL == nil {
		t.Fatal("Proxy returned nil URL")
	}
	if got := proxyURL.String(); got != "http://proxy.example.com:8080" {
		t.Fatalf("Proxy URL = %q, want %q", got, "http://proxy.example.com:8080")
	}
}

func TestApplyTransportDefaults_DisableHTTP2(t *testing.T) {
	tr := applyTransportDefaults(&http.Transport{}, true)
	if tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false when disable-http2 is enabled")
	}
	if tr.TLSNextProto == nil || len(tr.TLSNextProto) != 0 {
		t.Fatal("TLSNextProto should be an empty map when disable-http2 is enabled")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil, want explicit ALPN restriction when disable-http2 is enabled")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("TLSClientConfig.NextProtos = %v, want [http/1.1]", got)
	}
}

func TestResponseHeaderTimeoutForRequest_NewAPIHostGetsLongerTimeout(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://llm.whitedream.top/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if got := responseHeaderTimeoutForRequest(req); got != newAPIResponseHeaderTimeout {
		t.Fatalf("responseHeaderTimeoutForRequest() = %s, want %s", got, newAPIResponseHeaderTimeout)
	}
}

func TestApplyRequestTransportOverrides_AdjustsResponseHeaderTimeout(t *testing.T) {
	client := &http.Client{Transport: applyTransportDefaults(&http.Transport{}, false)}
	req, err := http.NewRequest(http.MethodPost, "https://llm.whitedream.top/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	applyRequestTransportOverrides(client, req)
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatal("transport = nil, want *http.Transport")
	}
	if tr.ResponseHeaderTimeout != newAPIResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %s, want %s", tr.ResponseHeaderTimeout, newAPIResponseHeaderTimeout)
	}
}

func TestShouldRetryUpstreamTransportError(t *testing.T) {
	if !shouldRetryUpstreamTransportError(timeoutErr("http2: timeout awaiting response headers")) {
		t.Fatal("expected timeout awaiting response headers to be retried")
	}
	if !shouldRetryUpstreamTransportError(timeoutErr(`Post "https://example.com/v1/messages": net/http: HTTP/1.x transport connection broken: malformed HTTP response "\x00\x00"`)) {
		t.Fatal("expected malformed HTTP response to be retried")
	}
	if !shouldRetryUpstreamTransportError(context.DeadlineExceeded) {
		t.Fatal("expected context deadline exceeded to be retried")
	}
	if !shouldRetryUpstreamTransportError(timeoutErr("write tcp4 10.0.0.1:12345->1.1.1.1:443: write: connection reset by peer")) {
		t.Fatal("expected connection reset by peer to be retried")
	}
	if !shouldRetryUpstreamTransportError(timeoutErr("write tcp4 10.0.0.1:12345->1.1.1.1:443: use of closed network connection")) {
		t.Fatal("expected use of closed network connection to be retried")
	}
	if shouldRetryUpstreamTransportError(io.EOF) {
		t.Fatal("unexpected retry for EOF")
	}
}

func TestAlternateHTTP2ModeForTransportError(t *testing.T) {
	tests := []struct {
		name          string
		disableHTTP2  bool
		err           error
		wantDisableH2 bool
		wantRetry     bool
	}{
		{
			name:          "http1_malformed_response_falls_back_to_http2",
			disableHTTP2:  true,
			err:           timeoutErr(`Post "https://example.com/v1/messages": net/http: HTTP/1.x transport connection broken: malformed HTTP response "\x00\x00"`),
			wantDisableH2: false,
			wantRetry:     true,
		},
		{
			name:          "http2_timeout_falls_back_to_http1",
			disableHTTP2:  false,
			err:           timeoutErr(`Post "https://example.com/v1/messages": http2: timeout awaiting response headers`),
			wantDisableH2: true,
			wantRetry:     true,
		},
		{
			name:          "connection_reset_stays_same_mode",
			disableHTTP2:  false,
			err:           timeoutErr(`write tcp4 10.0.0.1:12345->1.1.1.1:443: write: connection reset by peer`),
			wantDisableH2: false,
			wantRetry:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotDisableH2, gotRetry := alternateHTTP2ModeForTransportError(tc.disableHTTP2, tc.err)
			if gotDisableH2 != tc.wantDisableH2 || gotRetry != tc.wantRetry {
				t.Fatalf("alternateHTTP2ModeForTransportError() = (%v, %v), want (%v, %v)", gotDisableH2, gotRetry, tc.wantDisableH2, tc.wantRetry)
			}
		})
	}
}

func TestShouldFallbackYunyiClaudeTimeoutToHTTP1(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://cdn1.yunyi.cfd/claude/v1/messages", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	auth := &cliproxyauth.Auth{
		Prefix: "yunyi-claude",
		Attributes: map[string]string{
			"base_url": "https://cdn1.yunyi.cfd/claude",
		},
	}

	if !shouldFallbackYunyiClaudeTimeoutToHTTP1(req, auth, false, timeoutErr(`Post "https://cdn1.yunyi.cfd/claude/v1/messages": net/http: timeout awaiting response headers`)) {
		t.Fatal("expected yunyi claude timeout to trigger HTTP/1.1 fallback")
	}
	if shouldFallbackYunyiClaudeTimeoutToHTTP1(req, auth, true, timeoutErr(`Post "https://cdn1.yunyi.cfd/claude/v1/messages": net/http: timeout awaiting response headers`)) {
		t.Fatal("did not expect fallback when HTTP/2 is already disabled")
	}
	if shouldFallbackYunyiClaudeTimeoutToHTTP1(req, auth, false, timeoutErr(`write tcp4 10.0.0.1:12345->1.1.1.1:443: write: connection reset by peer`)) {
		t.Fatal("did not expect fallback for non-timeout transport errors")
	}
}

func TestEffectiveDisableHTTP2ForRequest_UsesHostCache(t *testing.T) {
	resetUpstreamHTTP2PreferenceStateForTest(t)
	t.Setenv("WRITABLE_PATH", t.TempDir())

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/messages", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableHTTP2: true}}
	if got := effectiveDisableHTTP2ForRequest(cfg, req); !got {
		t.Fatal("effectiveDisableHTTP2ForRequest() = false, want true from config default")
	}

	rememberDisableHTTP2ForRequest(req, false)
	if got := effectiveDisableHTTP2ForRequest(cfg, req); got {
		t.Fatal("effectiveDisableHTTP2ForRequest() = true, want cached false override")
	}
}

func TestEffectiveDisableHTTP2ForRequest_NewAPIHeadersPreferHTTP2(t *testing.T) {
	resetUpstreamHTTP2PreferenceStateForTest(t)
	t.Setenv("WRITABLE_PATH", t.TempDir())

	req, err := http.NewRequest(http.MethodPost, "https://llm.whitedream.top/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-NewAPI-Username", "test-user")

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableHTTP2: true}}
	if got := effectiveDisableHTTP2ForRequest(cfg, req); got {
		t.Fatal("effectiveDisableHTTP2ForRequest() = true, want false for NewAPI relay requests")
	}
}

func TestEffectiveDisableHTTP2ForRequest_HostCacheOverridesNewAPIPreference(t *testing.T) {
	resetUpstreamHTTP2PreferenceStateForTest(t)
	t.Setenv("WRITABLE_PATH", t.TempDir())

	req, err := http.NewRequest(http.MethodPost, "https://llm.whitedream.top/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-NewAPI-Username", "test-user")

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableHTTP2: true}}
	rememberDisableHTTP2ForRequest(req, true)
	if got := effectiveDisableHTTP2ForRequest(cfg, req); !got {
		t.Fatal("effectiveDisableHTTP2ForRequest() = false, want cached true override")
	}
}

func TestEffectiveDisableHTTP2ForRequest_LoadsPersistedState(t *testing.T) {
	resetUpstreamHTTP2PreferenceStateForTest(t)
	stateDir := t.TempDir()
	t.Setenv("WRITABLE_PATH", stateDir)

	path := filepath.Join(stateDir, ".cpapi-state", "upstream-http2-preferences.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	data, err := json.Marshal(persistedUpstreamHTTP2Preferences{
		UpdatedAt: time.Now().UTC(),
		Hosts: map[string]bool{
			"example.com": false,
		},
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/messages", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableHTTP2: true}}
	if got := effectiveDisableHTTP2ForRequest(cfg, req); got {
		t.Fatal("effectiveDisableHTTP2ForRequest() = true, want persisted false override")
	}
}

func TestRememberDisableHTTP2ForRequest_PersistsState(t *testing.T) {
	resetUpstreamHTTP2PreferenceStateForTest(t)
	stateDir := t.TempDir()
	t.Setenv("WRITABLE_PATH", stateDir)

	req, err := http.NewRequest(http.MethodPost, "https://persist.example.com/v1/messages", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	rememberDisableHTTP2ForRequest(req, false)

	path := filepath.Join(stateDir, ".cpapi-state", "upstream-http2-preferences.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	var state persistedUpstreamHTTP2Preferences
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal persisted state: %v", err)
	}
	if got, ok := state.Hosts["persist.example.com"]; !ok || got {
		t.Fatalf("persisted host mode = (%v, %v), want (false, true)", got, ok)
	}
}

func TestCloneRequestForRetry_ReplaysBody(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/messages", bytes.NewReader([]byte(`{"ok":true}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	retryReq, err := cloneRequestForRetry(req)
	if err != nil {
		t.Fatalf("cloneRequestForRetry: %v", err)
	}

	body, err := io.ReadAll(retryReq.Body)
	if err != nil {
		t.Fatalf("read retry body: %v", err)
	}
	if got := string(body); got != `{"ok":true}` {
		t.Fatalf("retry body = %q, want %q", got, `{"ok":true}`)
	}
}

func TestDoRequestWithTimeoutRetry_RetriesOnce(t *testing.T) {
	rt := &retryRoundTripper{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/messages", bytes.NewReader([]byte(`{"ping":"pong"}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := doRequestWithTimeoutRetry(ctx, nil, nil, req)
	if err != nil {
		t.Fatalf("doRequestWithTimeoutRetry: %v", err)
	}
	defer resp.Body.Close()
	if rt.calls != 2 {
		t.Fatalf("round trip calls = %d, want 2", rt.calls)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := string(body); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q) failed: %v", raw, err)
	}
	return parsed
}

type timeoutErr string

func (e timeoutErr) Error() string   { return string(e) }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

type retryRoundTripper struct {
	calls int
}

func (rt *retryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls++
	if rt.calls == 1 {
		return nil, timeoutErr("http2: timeout awaiting response headers")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func resetUpstreamHTTP2PreferenceStateForTest(t *testing.T) {
	t.Helper()
	upstreamHTTP2PreferenceCache = sync.Map{}
	upstreamHTTP2PreferenceLoadOnce = sync.Once{}
	upstreamHTTP2PreferenceStatePath = ""
}
