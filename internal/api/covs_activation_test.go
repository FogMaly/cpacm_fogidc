package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/covsactivation"
)

func TestRewriteCOVSModelPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":8}`))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	rewriteCOVSModelPrefix(c, "covs")

	var payload map[string]any
	if err := json.NewDecoder(c.Request.Body).Decode(&payload); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	if got := payload["model"]; got != "covs/claude-sonnet-4-6" {
		t.Fatalf("model = %v, want covs/claude-sonnet-4-6", got)
	}
}

func TestCOVSActivateRoute(t *testing.T) {
	server := newTestServer(t, &config.Config{
		Host: "127.0.0.1",
		Port: 34050,
		COVSActivation: config.COVSActivationConfig{
			Enable:                      true,
			PublicBaseURL:               "http://127.0.0.1:34050",
			DefaultClaudeProviderPrefix: "covs",
			Cards: []config.COVSActivationCard{
				{Code: "BTZO-4P80-ZGF3-DMOK", Product: "claude", Type: "month"},
			},
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/user/activate",
		strings.NewReader(`{"cardCode":"BTZO-4P80-ZGF3-DMOK","deviceId":"device-1","deviceInfo":{"platform":"linux"}}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}

	var resp covsactivation.ActivationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Product != "claude" {
		t.Fatalf("product = %q, want claude", resp.Product)
	}
	if resp.AnthropicBaseURL != "http://127.0.0.1:34050" {
		t.Fatalf("anthropicBaseUrl = %q", resp.AnthropicBaseURL)
	}
	if !strings.HasPrefix(resp.Token, "sk-covs-") {
		t.Fatalf("token = %q, want sk-covs-*", resp.Token)
	}
}
