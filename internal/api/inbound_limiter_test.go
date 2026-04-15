package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestInboundLimiter_PerKeyQPS(t *testing.T) {
	limiter := newInboundLimiter(&config.Config{
		InboundRateLimit: config.InboundRateLimitConfig{PerKeyQPS: 1},
	})
	if !limiter.AllowKey("key-1") {
		t.Fatal("first AllowKey should pass")
	}
	if limiter.AllowKey("key-1") {
		t.Fatal("second AllowKey should be rate limited")
	}
	if !limiter.AllowKey("key-2") {
		t.Fatal("different key should have independent bucket")
	}
}

func TestGlobalConcurrencyMiddleware_RejectsWhenFull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	limiter := newInboundLimiter(&config.Config{
		InboundRateLimit: config.InboundRateLimitConfig{GlobalConcurrency: 1},
	})
	release, ok := limiter.AcquireGlobal()
	if !ok || release == nil {
		t.Fatal("failed to seed in-flight request")
	}
	defer release()

	router := gin.New()
	router.Use(globalConcurrencyMiddleware(limiter))
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
}
