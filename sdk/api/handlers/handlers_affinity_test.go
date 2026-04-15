package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestRequestExecutionMetadata_IncludesSyntheticSessionAffinitySeed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer test-proxy-key")
	req.Header.Set("User-Agent", "sticky-client/1.0")
	req.Header.Set("X-Forwarded-For", "198.51.100.24")
	ginCtx.Request = req

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	meta := requestExecutionMetadata(ctx)

	rawSeed, ok := meta[coreexecutor.SessionAffinitySeedMetadataKey].(string)
	if !ok || strings.TrimSpace(rawSeed) == "" {
		t.Fatalf("session affinity seed missing from metadata: %#v", meta)
	}
	if !strings.HasPrefix(rawSeed, "fallback:") {
		t.Fatalf("session affinity seed = %q, want fallback:*", rawSeed)
	}
	if strings.Contains(rawSeed, "test-proxy-key") {
		t.Fatalf("session affinity seed leaked raw auth token: %q", rawSeed)
	}
}

func TestRequestExecutionMetadata_SyntheticSessionAffinitySeedIgnoresHost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	buildSeed := func(host string) string {
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)

		req := httptest.NewRequest(http.MethodPost, "https://"+host+"/v1/chat/completions", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer test-proxy-key")
		req.Header.Set("User-Agent", "sticky-client/1.0")
		req.Header.Set("X-Forwarded-For", "198.51.100.24")
		ginCtx.Request = req

		ctx := context.WithValue(context.Background(), "gin", ginCtx)
		meta := requestExecutionMetadata(ctx)
		seed, _ := meta[coreexecutor.SessionAffinitySeedMetadataKey].(string)
		return seed
	}

	seedFromFogIDC := buildSeed("ysjf.fogidc.com")
	seedFromOtherHost := buildSeed("example.com")
	if strings.TrimSpace(seedFromFogIDC) == "" {
		t.Fatal("seedFromFogIDC = empty, want non-empty")
	}
	if seedFromFogIDC != seedFromOtherHost {
		t.Fatalf("synthetic session seed changed with host: fogidc=%q other=%q", seedFromFogIDC, seedFromOtherHost)
	}
}

func TestRequestExecutionMetadata_SyntheticSessionAffinitySeedChangesWithForwardedClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	buildSeed := func(clientIP string) string {
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)

		req := httptest.NewRequest(http.MethodPost, "https://ysjf.fogidc.com/v1/chat/completions", nil)
		req.Host = "ysjf.fogidc.com"
		req.Header.Set("Authorization", "Bearer test-proxy-key")
		req.Header.Set("User-Agent", "sticky-client/1.0")
		req.Header.Set("X-Forwarded-For", clientIP)
		ginCtx.Request = req

		ctx := context.WithValue(context.Background(), "gin", ginCtx)
		meta := requestExecutionMetadata(ctx)
		seed, _ := meta[coreexecutor.SessionAffinitySeedMetadataKey].(string)
		return seed
	}

	seedA := buildSeed("198.51.100.24")
	seedB := buildSeed("198.51.100.77")
	if strings.TrimSpace(seedA) == "" || strings.TrimSpace(seedB) == "" {
		t.Fatalf("unexpected empty seeds: seedA=%q seedB=%q", seedA, seedB)
	}
	if seedA == seedB {
		t.Fatalf("synthetic session seed did not change with forwarded client IP: %q", seedA)
	}
}

func TestRequestExecutionHeaders_ClonesIncomingHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Session-Id", "sess-123")
	req.Header.Set("Conversation-Id", "conv-456")
	ginCtx.Request = req

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	headers := requestExecutionHeaders(ctx)
	if headers == nil {
		t.Fatal("requestExecutionHeaders() = nil")
	}
	if got := headers.Get("Session-Id"); got != "sess-123" {
		t.Fatalf("Session-Id = %q, want %q", got, "sess-123")
	}
	if got := headers.Get("Conversation-Id"); got != "conv-456" {
		t.Fatalf("Conversation-Id = %q, want %q", got, "conv-456")
	}

	headers.Set("Session-Id", "mutated")
	if got := ginCtx.Request.Header.Get("Session-Id"); got != "sess-123" {
		t.Fatalf("original request header mutated to %q, want %q", got, "sess-123")
	}
}
