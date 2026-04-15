package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestRuntimeStatusRouteRequiresManagementKeyAndReturnsPayload(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/v0/management/runtime-status", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v0/management/runtime-status without key status = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/runtime-status?management_key=mgmt-key", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v0/management/runtime-status with key status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var payload struct {
		Process struct {
			PID    int `json:"pid"`
			Memory struct {
				AllocBytes uint64 `json:"alloc_bytes"`
			} `json:"memory"`
			ServerConfig struct {
				LoggingToFile     bool `json:"logging_to_file"`
				RequestLogEnabled bool `json:"request_log_enabled"`
			} `json:"server_config"`
		} `json:"process"`
		Usage struct {
			DispatchQueue struct {
				Capacity int `json:"capacity"`
			} `json:"dispatch_queue"`
		} `json:"usage"`
		InboundRateLimit struct {
			Enabled           bool    `json:"enabled"`
			PerKeyQPS         float64 `json:"per_key_qps"`
			GlobalConcurrency int     `json:"global_concurrency"`
		} `json:"inbound_rate_limit"`
		Logs struct {
			MaxTotalSizeMB int `json:"max_total_size_mb"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if payload.Process.PID <= 0 {
		t.Fatalf("process.pid = %d, want > 0", payload.Process.PID)
	}
	if payload.Usage.DispatchQueue.Capacity <= 0 {
		t.Fatalf("usage.dispatch_queue.capacity = %d, want > 0", payload.Usage.DispatchQueue.Capacity)
	}
	if !payload.InboundRateLimit.Enabled {
		t.Fatal("inbound_rate_limit.enabled = false, want true")
	}
	if payload.InboundRateLimit.GlobalConcurrency != 3 {
		t.Fatalf("inbound_rate_limit.global_concurrency = %d, want 3", payload.InboundRateLimit.GlobalConcurrency)
	}
	if payload.InboundRateLimit.PerKeyQPS != 2 {
		t.Fatalf("inbound_rate_limit.per_key_qps = %v, want 2", payload.InboundRateLimit.PerKeyQPS)
	}
	if !payload.Process.ServerConfig.LoggingToFile {
		t.Fatal("process.server_config.logging_to_file = false, want true")
	}
	if payload.Process.ServerConfig.RequestLogEnabled {
		t.Fatal("process.server_config.request_log_enabled = true, want false")
	}
	if payload.Logs.MaxTotalSizeMB != 512 {
		t.Fatalf("logs.max_total_size_mb = %d, want 512", payload.Logs.MaxTotalSizeMB)
	}
}
