package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type serverTestRequestLogger struct {
	enabled    bool
	forceCalls int
}

func (l *serverTestRequestLogger) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return nil
}

func (l *serverTestRequestLogger) LogRequestWithOptions(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, force bool, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	if force {
		l.forceCalls++
	}
	return nil
}

func (l *serverTestRequestLogger) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (logging.StreamingLogWriter, error) {
	return &logging.NoOpStreamingLogWriter{}, nil
}

func (l *serverTestRequestLogger) IsEnabled() bool { return l.enabled }

func TestNewServer_CommercialModeStillEnablesErrorRequestLogging(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys:    []string{"test-key"},
			RequestLog: false,
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          true,
		UsageStatisticsEnabled: false,
		CommercialMode:         true,
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()
	configPath := filepath.Join(tmpDir, "config.yaml")
	testLogger := &serverTestRequestLogger{enabled: false}

	server := NewServer(
		cfg,
		authManager,
		accessManager,
		configPath,
		WithRequestLoggerFactory(func(*proxyconfig.Config, string) logging.RequestLogger {
			return testLogger
		}),
		WithRouterConfigurator(func(engine *gin.Engine, _ *handlers.BaseAPIHandler, _ *proxyconfig.Config) {
			engine.POST("/test/failure", func(c *gin.Context) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "boom"})
			})
		}),
	)

	req := httptest.NewRequest(http.MethodPost, "/test/failure", nil)
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if testLogger.forceCalls != 1 {
		t.Fatalf("forceCalls = %d, want 1", testLogger.forceCalls)
	}
}
