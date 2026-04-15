package executor

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

const defaultUpstreamResponseHeaderTimeout = 30 * time.Second
const newAPIResponseHeaderTimeout = 75 * time.Second

var (
	upstreamHTTP2PreferenceCache     sync.Map
	upstreamHTTP2PreferenceLoadOnce  sync.Once
	upstreamHTTP2PreferencePersistMu sync.Mutex
	upstreamHTTP2PreferenceStatePath string
)

type persistedUpstreamHTTP2Preferences struct {
	UpdatedAt time.Time       `json:"updated_at"`
	Hosts     map[string]bool `json:"hosts"`
}

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured
// 3. Use RoundTripper from context if neither are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	disableHTTP2 := cfg != nil && cfg.DisableHTTP2
	return newProxyAwareHTTPClientWithMode(ctx, cfg, auth, timeout, disableHTTP2)
}

func newProxyAwareHTTPClientWithMode(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration, disableHTTP2 bool) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := buildProxyTransport(proxyURL, disableHTTP2)
		if transport != nil {
			httpClient.Transport = transport
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = maybeDisableHTTP2RoundTripper(rt, disableHTTP2)
		return httpClient
	}

	// Prefer IPv4 for direct connections; fall back to the original network
	// when IPv4 is unavailable.
	httpClient.Transport = newIPv4PreferredTransport(disableHTTP2)

	return httpClient
}

func effectiveDisableHTTP2ForRequest(cfg *config.Config, req *http.Request) bool {
	loadPersistedUpstreamHTTP2Preferences()
	disableHTTP2 := cfg != nil && cfg.DisableHTTP2
	host := requestHostForProtocolCache(req)
	if host == "" {
		if disableHTTP2 && shouldPreferHTTP2ForRequest(req) {
			return false
		}
		return disableHTTP2
	}
	if cached, ok := upstreamHTTP2PreferenceCache.Load(host); ok {
		if override, ok := cached.(bool); ok {
			return override
		}
	}
	if disableHTTP2 && shouldPreferHTTP2ForRequest(req) {
		return false
	}
	return disableHTTP2
}

func shouldPreferHTTP2ForRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	for key := range req.Header {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "x-newapi-") {
			return true
		}
	}
	host := requestHostForProtocolCache(req)
	return strings.Contains(host, "whitedream.top")
}

func rememberDisableHTTP2ForRequest(req *http.Request, disableHTTP2 bool) {
	loadPersistedUpstreamHTTP2Preferences()
	host := requestHostForProtocolCache(req)
	if host == "" {
		return
	}
	if cached, ok := upstreamHTTP2PreferenceCache.Load(host); ok {
		if current, ok := cached.(bool); ok && current == disableHTTP2 {
			return
		}
	}
	upstreamHTTP2PreferenceCache.Store(host, disableHTTP2)
	persistUpstreamHTTP2Preferences()
}

func requestHostForProtocolCache(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	host := strings.ToLower(strings.TrimSpace(req.URL.Host))
	if host == "" {
		return ""
	}
	return host
}

func loadPersistedUpstreamHTTP2Preferences() {
	upstreamHTTP2PreferenceLoadOnce.Do(func() {
		path := upstreamHTTP2PreferenceFilePath()
		upstreamHTTP2PreferenceStatePath = path
		if path == "" {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) == 0 {
			return
		}
		var state persistedUpstreamHTTP2Preferences
		if err := json.Unmarshal(data, &state); err != nil {
			log.WithError(err).Warnf("failed to parse persisted upstream HTTP mode preferences: %s", path)
			return
		}
		for host, disableHTTP2 := range state.Hosts {
			host = strings.ToLower(strings.TrimSpace(host))
			if host == "" {
				continue
			}
			upstreamHTTP2PreferenceCache.Store(host, disableHTTP2)
		}
	})
}

func persistUpstreamHTTP2Preferences() {
	upstreamHTTP2PreferencePersistMu.Lock()
	defer upstreamHTTP2PreferencePersistMu.Unlock()

	path := upstreamHTTP2PreferenceStatePath
	if path == "" {
		path = upstreamHTTP2PreferenceFilePath()
		upstreamHTTP2PreferenceStatePath = path
	}
	if path == "" {
		return
	}

	state := persistedUpstreamHTTP2Preferences{
		UpdatedAt: time.Now().UTC(),
		Hosts:     make(map[string]bool),
	}
	upstreamHTTP2PreferenceCache.Range(func(key, value any) bool {
		host, okHost := key.(string)
		disableHTTP2, okMode := value.(bool)
		if !okHost || !okMode {
			return true
		}
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			return true
		}
		state.Hosts[host] = disableHTTP2
		return true
	})

	if len(state.Hosts) == 0 {
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.WithError(err).Warnf("failed to create upstream HTTP mode state dir: %s", filepath.Dir(path))
		return
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.WithError(err).Warn("failed to marshal upstream HTTP mode preferences")
		return
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		log.WithError(err).Warnf("failed to write upstream HTTP mode preferences: %s", tmpPath)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		log.WithError(err).Warnf("failed to finalize upstream HTTP mode preferences: %s", path)
		_ = os.Remove(tmpPath)
	}
}

func upstreamHTTP2PreferenceFilePath() string {
	base := util.WritablePath()
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		base = wd
	}
	return filepath.Join(base, ".cpapi-state", "upstream-http2-preferences.json")
}

func newIPv4PreferredTransport(disableHTTP2 bool) *http.Transport {
	base, _ := http.DefaultTransport.(*http.Transport)
	if base == nil {
		base = &http.Transport{}
	}
	tr := base.Clone()
	tr.Proxy = nil
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "" {
			network = "tcp"
		}
		if strings.HasPrefix(network, "tcp") {
			if conn, err := dialer.DialContext(ctx, "tcp4", addr); err == nil {
				return conn, nil
			}
		}
		return dialer.DialContext(ctx, network, addr)
	}
	return applyTransportDefaults(tr, disableHTTP2)
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string, disableHTTP2 bool) *http.Transport {
	if proxyURL == "" {
		return nil
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("parse proxy URL failed: %v", errParse)
		return nil
	}

	base, _ := http.DefaultTransport.(*http.Transport)
	if base == nil {
		base = &http.Transport{}
	}
	transport := base.Clone()

	// Handle different proxy schemes
	if parsedURL.Scheme == "socks5" {
		transport.Proxy = nil
		// Configure SOCKS5 proxy with optional authentication
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
			return nil
		}
		// Set up a custom transport using the SOCKS5 dialer
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	} else if parsedURL.Scheme == "http" || parsedURL.Scheme == "https" {
		// Configure HTTP or HTTPS proxy
		transport.Proxy = http.ProxyURL(parsedURL)
	} else {
		log.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
		return nil
	}

	return applyTransportDefaults(transport, disableHTTP2)
}

func applyTransportDefaults(tr *http.Transport, disableHTTP2 bool) *http.Transport {
	if tr == nil {
		tr = &http.Transport{}
	}
	if tr.TLSHandshakeTimeout <= 0 {
		tr.TLSHandshakeTimeout = 10 * time.Second
	}
	if tr.ExpectContinueTimeout <= 0 {
		tr.ExpectContinueTimeout = time.Second
	}
	if tr.IdleConnTimeout <= 0 {
		tr.IdleConnTimeout = 90 * time.Second
	}
	if tr.ResponseHeaderTimeout <= 0 {
		tr.ResponseHeaderTimeout = defaultUpstreamResponseHeaderTimeout
	}
	if disableHTTP2 {
		tr.ForceAttemptHTTP2 = false
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		} else {
			tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		}
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	} else {
		tr.ForceAttemptHTTP2 = true
		if tr.TLSClientConfig != nil {
			tr.TLSClientConfig = tr.TLSClientConfig.Clone()
			tr.TLSClientConfig.NextProtos = nil
		}
		if tr.TLSNextProto != nil && len(tr.TLSNextProto) == 0 {
			tr.TLSNextProto = nil
		}
		if err := http2.ConfigureTransport(tr); err != nil && !strings.Contains(strings.ToLower(err.Error()), "protocol https already registered") {
			log.WithError(err).Debug("failed to configure explicit HTTP/2 transport")
		}
	}
	return tr
}

func responseHeaderTimeoutForRequest(req *http.Request) time.Duration {
	if shouldPreferHTTP2ForRequest(req) {
		return newAPIResponseHeaderTimeout
	}
	return defaultUpstreamResponseHeaderTimeout
}

func applyRequestTransportOverrides(client *http.Client, req *http.Request) {
	if client == nil {
		return
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr == nil {
		return
	}
	timeout := responseHeaderTimeoutForRequest(req)
	if timeout <= 0 || tr.ResponseHeaderTimeout == timeout {
		return
	}
	clone := tr.Clone()
	clone.ResponseHeaderTimeout = timeout
	client.Transport = clone
}

func maybeDisableHTTP2RoundTripper(rt http.RoundTripper, disableHTTP2 bool) http.RoundTripper {
	if !disableHTTP2 || rt == nil {
		return rt
	}
	tr, ok := rt.(*http.Transport)
	if !ok || tr == nil {
		return rt
	}
	return applyTransportDefaults(tr.Clone(), true)
}

func doRequestWithTimeoutRetry(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	preferredDisableHTTP2 := effectiveDisableHTTP2ForRequest(cfg, req)
	client := newProxyAwareHTTPClientWithMode(ctx, cfg, auth, 0, preferredDisableHTTP2)
	applyRequestTransportOverrides(client, req)
	firstStart := time.Now()
	resp, err := client.Do(req)
	if err == nil {
		rememberDisableHTTP2ForRequest(req, preferredDisableHTTP2)
		return resp, err
	}
	if shouldFallbackYunyiClaudeTimeoutToHTTP1(req, auth, preferredDisableHTTP2, err) {
		retryReq, retryErr := cloneRequestForRetry(req)
		if retryErr != nil {
			logWithRequestID(ctx).WithFields(log.Fields{
				"url":               requestURLForTimeoutLog(req),
				"elapsed_ms":        time.Since(firstStart).Milliseconds(),
				"retry_clone_error": retryErr.Error(),
			}).Warnf("yunyi claude upstream timeout fallback was skipped after transport error: %v", err)
			return nil, err
		}
		logWithRequestID(ctx).WithFields(log.Fields{
			"url":            requestURLForTimeoutLog(req),
			"elapsed_ms":     time.Since(firstStart).Milliseconds(),
			"disable_http2":  preferredDisableHTTP2,
			"fallback_http1": true,
		}).Warnf("yunyi claude upstream timeout detected, retrying with HTTP/1.1: %v", err)
		rememberDisableHTTP2ForRequest(req, true)
		retryClient := newProxyAwareHTTPClientWithMode(ctx, cfg, auth, 0, true)
		applyRequestTransportOverrides(retryClient, retryReq)
		retryStart := time.Now()
		retryResp, retryErr := retryClient.Do(retryReq)
		if retryErr == nil {
			return retryResp, nil
		}
		logWithRequestID(ctx).WithFields(log.Fields{
			"url":            requestURLForTimeoutLog(retryReq),
			"elapsed_ms":     time.Since(retryStart).Milliseconds(),
			"disable_http2":  true,
			"fallback_http1": true,
		}).Warnf("yunyi claude HTTP/1.1 retry failed: %v", retryErr)
		return retryResp, retryErr
	}
	if alternateDisableHTTP2, ok := alternateHTTP2ModeForTransportError(preferredDisableHTTP2, err); ok {
		retryReq, retryErr := cloneRequestForRetry(req)
		if retryErr != nil {
			logWithRequestID(ctx).WithFields(log.Fields{
				"url":               requestURLForTimeoutLog(req),
				"elapsed_ms":        time.Since(firstStart).Milliseconds(),
				"retry_clone_error": retryErr.Error(),
			}).Warnf("upstream protocol fallback was skipped after transport error: %v", err)
			return nil, err
		}
		logWithRequestID(ctx).WithFields(log.Fields{
			"url":            requestURLForTimeoutLog(req),
			"elapsed_ms":     time.Since(firstStart).Milliseconds(),
			"disable_http2":  preferredDisableHTTP2,
			"fallback_http1": alternateDisableHTTP2,
		}).Warnf("upstream transport protocol mismatch detected, retrying with alternate HTTP mode: %v", err)
		rememberDisableHTTP2ForRequest(req, alternateDisableHTTP2)
		retryClient := newProxyAwareHTTPClientWithMode(ctx, cfg, auth, 0, alternateDisableHTTP2)
		applyRequestTransportOverrides(retryClient, retryReq)
		retryStart := time.Now()
		retryResp, retryErr := retryClient.Do(retryReq)
		if retryErr == nil {
			return retryResp, nil
		}
		logWithRequestID(ctx).WithFields(log.Fields{
			"url":            requestURLForTimeoutLog(retryReq),
			"elapsed_ms":     time.Since(retryStart).Milliseconds(),
			"disable_http2":  alternateDisableHTTP2,
			"fallback_http1": alternateDisableHTTP2,
		}).Warnf("upstream alternate HTTP mode retry failed: %v", retryErr)
		return retryResp, retryErr
	}
	if !shouldRetryUpstreamTransportError(err) {
		return resp, err
	}

	retryReq, retryErr := cloneRequestForRetry(req)
	if retryErr != nil {
		logWithRequestID(ctx).WithFields(log.Fields{
			"url":               requestURLForTimeoutLog(req),
			"elapsed_ms":        time.Since(firstStart).Milliseconds(),
			"retry_clone_error": retryErr.Error(),
		}).Warnf("upstream transport error occurred and retry was skipped: %v", err)
		return nil, err
	}

	logWithRequestID(ctx).WithFields(log.Fields{
		"url":        requestURLForTimeoutLog(req),
		"elapsed_ms": time.Since(firstStart).Milliseconds(),
	}).Warnf("upstream transport error occurred, retrying once: %v", err)
	retryClient := newProxyAwareHTTPClientWithMode(ctx, cfg, auth, 0, preferredDisableHTTP2)
	applyRequestTransportOverrides(retryClient, retryReq)
	retryStart := time.Now()
	retryResp, retryErr := retryClient.Do(retryReq)
	if retryErr != nil {
		logWithRequestID(ctx).WithFields(log.Fields{
			"url":        requestURLForTimeoutLog(retryReq),
			"elapsed_ms": time.Since(retryStart).Milliseconds(),
		}).Warnf("upstream retry failed: %v", retryErr)
		return retryResp, retryErr
	}
	rememberDisableHTTP2ForRequest(req, preferredDisableHTTP2)
	return retryResp, nil
}

func shouldRetryUpstreamTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout awaiting response headers") ||
		strings.Contains(msg, "client.timeout exceeded") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "malformed http response") ||
		strings.Contains(msg, "http/1.x transport connection broken")
}

func alternateHTTP2ModeForTransportError(disableHTTP2 bool, err error) (bool, bool) {
	if err == nil {
		return false, false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if msg == "" {
		return false, false
	}
	switch {
	case disableHTTP2 && (strings.Contains(msg, "malformed http response") ||
		strings.Contains(msg, "http/1.x transport connection broken")):
		return false, true
	case !disableHTTP2 && strings.Contains(msg, "http2: timeout awaiting response headers"):
		return true, true
	default:
		return false, false
	}
}

func cloneRequestForRetry(req *http.Request) (*http.Request, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	retryReq := req.Clone(req.Context())
	if req.Body == nil {
		return retryReq, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("request body is not replayable")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	retryReq.Body = body
	retryReq.GetBody = req.GetBody
	retryReq.ContentLength = req.ContentLength
	return retryReq, nil
}

func requestURLForTimeoutLog(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "<unknown>"
	}
	return req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
}

func shouldFallbackYunyiClaudeTimeoutToHTTP1(req *http.Request, auth *cliproxyauth.Auth, disableHTTP2 bool, err error) bool {
	if disableHTTP2 || err == nil || req == nil || req.URL == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if msg == "" || !strings.Contains(msg, "timeout awaiting response headers") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(req.URL.Hostname()), "cdn1.yunyi.cfd") && strings.Contains(strings.ToLower(strings.TrimSpace(req.URL.Path)), "/claude/") {
		return true
	}
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Prefix), "yunyi-claude") {
		return true
	}
	baseURL := strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	return strings.Contains(baseURL, "cdn1.yunyi.cfd/claude")
}
