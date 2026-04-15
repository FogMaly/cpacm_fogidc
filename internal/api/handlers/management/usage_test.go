package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func writePersistedUsageDetail(t *testing.T, dir string, ts time.Time) {
	t.Helper()

	body := fmt.Sprintf(
		`{"timestamp":"%s","api_key":"codex","model":"gpt-5.4","source":"unit","auth_index":"0","failed":false,"tokens":{"total_tokens":42}}`+"\n",
		ts.UTC().Format(time.RFC3339),
	)
	filename := filepath.Join(dir, ts.UTC().Format("2006-01-02")+".jsonl")
	if err := os.WriteFile(filename, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func TestGetUsageStatistics_MergesPersistedDetailsWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	writePersistedUsageDetail(t, dir, time.Now().UTC().Add(-2*time.Hour))

	usage.ConfigureRuntime(usage.RuntimeConfig{
		Enabled:             true,
		DetailsStateDir:     dir,
		DetailsMemoryWindow: time.Hour,
		DetailsRetention:    7 * 24 * time.Hour,
	})
	defer usage.ConfigureRuntime(usage.RuntimeConfig{})

	stats := usage.NewRequestStatistics()
	handler := &Handler{usageStats: stats}

	req := httptest.NewRequest(http.MethodGet, "/usage?details_hours=24", nil)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = req

	handler.GetUsageStatistics(c)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	usagePayload, _ := payload["usage"].(map[string]any)
	apis, _ := usagePayload["apis"].(map[string]any)
	codex, _ := apis["codex"].(map[string]any)
	models, _ := codex["models"].(map[string]any)
	model, _ := models["gpt-5.4"].(map[string]any)
	details, _ := model["details"].([]any)
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
	persisted, _ := payload["persisted"].(map[string]any)
	if got := int(persisted["loaded_records"].(float64)); got != 1 {
		t.Fatalf("persisted.loaded_records = %d, want 1", got)
	}
}

func TestExportUsageStatistics_IncludesPersistedDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	writePersistedUsageDetail(t, dir, time.Now().UTC().Add(-2*time.Hour))

	usage.ConfigureRuntime(usage.RuntimeConfig{
		Enabled:             true,
		DetailsStateDir:     dir,
		DetailsMemoryWindow: 15 * time.Minute,
		DetailsRetention:    7 * 24 * time.Hour,
	})
	defer usage.ConfigureRuntime(usage.RuntimeConfig{})

	handler := &Handler{usageStats: usage.NewRequestStatistics()}

	req := httptest.NewRequest(http.MethodGet, "/usage/export", nil)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = req

	handler.ExportUsageStatistics(c)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var payload usageExportPayload
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	details := payload.Usage.APIs["codex"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
}
