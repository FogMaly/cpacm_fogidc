package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func writeProbeTestScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script %s: %v", path, err)
	}
}

func TestReadProbeSchedulerSettingsReadsProbeEnvOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}

	envPath := filepath.Join(scriptsDir, "model-health.env")
	if err := os.WriteFile(envPath, []byte(strings.Join([]string{
		"CHECK_INTERVAL_SEC=1800",
		"PROBE_RETRY_COUNT=2",
		"PROBE_MAX_RETRY_WAIT_SEC=7",
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatalf("write env: %v", err)
	}
	writeProbeTestScript(t, filepath.Join(scriptsDir, "model-health-status.sh"), "#!/usr/bin/env bash\necho 'scheduler: running'\n")

	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	h := NewHandler(&config.Config{
		RequestRetry:     0,
		MaxRetryInterval: 3,
	}, configPath, nil)

	settings, err := h.readProbeSchedulerSettings()
	if err != nil {
		t.Fatalf("readProbeSchedulerSettings() error = %v", err)
	}
	if !settings.Enabled {
		t.Fatalf("settings.Enabled = false, want true")
	}
	if settings.IntervalMinutes != 30 {
		t.Fatalf("settings.IntervalMinutes = %d, want 30", settings.IntervalMinutes)
	}
	if settings.RetryCount != 2 {
		t.Fatalf("settings.RetryCount = %d, want 2", settings.RetryCount)
	}
	if settings.MaxRetryWaitSeconds != 7 {
		t.Fatalf("settings.MaxRetryWaitSeconds = %d, want 7", settings.MaxRetryWaitSeconds)
	}
}

func TestPutProbeSchedulerWritesProbeEnvValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	scriptsDir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}

	envPath := filepath.Join(scriptsDir, "model-health.env")
	if err := os.WriteFile(envPath, []byte(strings.Join([]string{
		"CHECK_INTERVAL_SEC=1800",
		"PROBE_RETRY_COUNT=0",
		"PROBE_MAX_RETRY_WAIT_SEC=3",
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatalf("write env: %v", err)
	}
	writeProbeTestScript(t, filepath.Join(scriptsDir, "model-health-status.sh"), "#!/usr/bin/env bash\necho 'scheduler: running'\n")

	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	h := NewHandler(&config.Config{}, configPath, nil)

	body := map[string]any{
		"retryCount":          4,
		"maxRetryWaitSeconds": 9,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/probe-scheduler", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutProbeScheduler(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	updatedEnv, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	text := string(updatedEnv)
	if !strings.Contains(text, "PROBE_RETRY_COUNT=4") {
		t.Fatalf("updated env missing retry count: %s", text)
	}
	if !strings.Contains(text, "PROBE_MAX_RETRY_WAIT_SEC=9") {
		t.Fatalf("updated env missing max retry wait: %s", text)
	}
	if !strings.Contains(text, "CHECK_INTERVAL_SEC=1800") {
		t.Fatalf("updated env changed interval unexpectedly: %s", text)
	}
}
