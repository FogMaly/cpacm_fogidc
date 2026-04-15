package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
)

type middlewareTestLogger struct {
	enabled    bool
	forceCalls int
	lastBody   []byte
	lastResp   []byte
}

func (l *middlewareTestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	l.lastBody = bytes.Clone(body)
	l.lastResp = bytes.Clone(response)
	return nil
}

func (l *middlewareTestLogger) LogRequestWithOptions(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	l.lastBody = bytes.Clone(body)
	l.lastResp = bytes.Clone(response)
	if force {
		l.forceCalls++
	}
	return nil
}

func (l *middlewareTestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (logging.StreamingLogWriter, error) {
	return &logging.NoOpStreamingLogWriter{}, nil
}

func (l *middlewareTestLogger) IsEnabled() bool {
	return l.enabled
}

func TestRequestLoggingMiddleware_ForcesErrorLoggingWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &middlewareTestLogger{enabled: false}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger, nil))
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "boom"})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if logger.forceCalls != 1 {
		t.Fatalf("forceCalls = %d, want 1", logger.forceCalls)
	}
}

func TestRequestLoggingMiddleware_TruncatesCapturedBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &middlewareTestLogger{enabled: true}
	engine := gin.New()
	cfg := &config.SDKConfig{RequestLogMaxBodyBytes: 8}
	engine.Use(RequestLoggingMiddleware(logger, cfg))
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", []byte("1234567890"))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString("abcdefghij"))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if !bytes.Contains(logger.lastBody, []byte("abcdefgh")) {
		t.Fatalf("captured request body missing prefix: %q", logger.lastBody)
	}
	if bytes.Contains(logger.lastBody, []byte("abcdefghij")) {
		t.Fatalf("captured request body should be truncated: %q", logger.lastBody)
	}
	if !bytes.Contains(logger.lastBody, []byte("[client request body truncated after 8 bytes]")) {
		t.Fatalf("captured request body missing truncation marker: %q", logger.lastBody)
	}
	if !bytes.Contains(logger.lastResp, []byte("12345678")) {
		t.Fatalf("captured response body missing prefix: %q", logger.lastResp)
	}
	if bytes.Contains(logger.lastResp, []byte("1234567890")) {
		t.Fatalf("captured response body should be truncated: %q", logger.lastResp)
	}
	if !bytes.Contains(logger.lastResp, []byte("[client response body truncated after 8 bytes]")) {
		t.Fatalf("captured response body missing truncation marker: %q", logger.lastResp)
	}
}

func TestRequestLoggingMiddleware_PreservesFullRequestBodyForHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &middlewareTestLogger{enabled: true}
	engine := gin.New()
	cfg := &config.SDKConfig{RequestLogMaxBodyBytes: 8}
	engine.Use(RequestLoggingMiddleware(logger, cfg))
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Fatalf("handler failed to read body: %v", err)
		}
		c.Data(http.StatusOK, "text/plain", body)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString("abcdefghij"))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Body.String(); got != "abcdefghij" {
		t.Fatalf("handler body = %q, want %q", got, "abcdefghij")
	}
	if !bytes.Contains(logger.lastBody, []byte("abcdefgh")) {
		t.Fatalf("captured request body missing prefix: %q", logger.lastBody)
	}
	if bytes.Contains(logger.lastBody, []byte("abcdefghij")) {
		t.Fatalf("captured request body should stay truncated: %q", logger.lastBody)
	}
}
