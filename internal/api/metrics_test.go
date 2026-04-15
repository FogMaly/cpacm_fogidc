package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestMetricsRouteRequiresManagementKeyAndReturnsPrometheusText(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "mgmt-key")
	server := newTestServer(t)
	server.cfg.LoggingToFile = true
	server.cfg.RequestLog = false
	server.cfg.LogsMaxTotalSizeMB = 512
	server.cfg.InboundRateLimit = config.InboundRateLimitConfig{
		PerKeyQPS:         2,
		GlobalConcurrency: 3,
	}
	server.inboundLimiter = newInboundLimiter(server.cfg)

	req := httptest.NewRequest(http.MethodGet, "/v0/management/metrics", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v0/management/metrics without key status = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/metrics?management_key=mgmt-key", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v0/management/metrics with key status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	for _, want := range []string{
		"# HELP cpapi_process_goroutines",
		"# TYPE cpapi_process_goroutines gauge",
		"cpapi_process_goroutines ",
		"cpapi_config_request_log_enabled 0.000000",
		"cpapi_inbound_rate_limit_global_concurrency 3.000000",
		"cpapi_logs_max_total_size_megabytes 512.000000",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q: %s", want, body)
		}
	}
}
