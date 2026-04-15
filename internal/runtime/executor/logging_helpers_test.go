package executor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestAppendAPIResponseChunk_TruncatesStreamingLogBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			RequestLog:               true,
			RequestLogStreamMaxBytes: 16,
		},
	}

	appendAPIResponseChunk(ctx, cfg, []byte("1234567890"))
	appendAPIResponseChunk(ctx, cfg, []byte("abcdefghij"))
	appendAPIResponseChunk(ctx, cfg, []byte("ignored-after-cap"))

	data := MaterializeAPIResponse(ginCtx)
	if len(data) == 0 {
		t.Fatal("MaterializeAPIResponse() returned empty data")
	}
	text := string(data)
	if !strings.Contains(text, "1234567890") {
		t.Fatalf("response log missing first chunk: %q", text)
	}
	if !strings.Contains(text, "abcdef") {
		t.Fatalf("response log missing truncated second chunk prefix: %q", text)
	}
	if strings.Contains(text, "ignored-after-cap") {
		t.Fatalf("response log should not contain data beyond cap: %q", text)
	}
	if strings.Count(text, "[stream log truncated after 16 bytes]") != 1 {
		t.Fatalf("truncation marker count mismatch: %q", text)
	}
}

func TestAppendAPIResponseLog_TruncatesNonStreamingBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx := &gin.Context{}

	AppendAPIResponseLog(ginCtx, []byte("12345"), 8)
	AppendAPIResponseLog(ginCtx, []byte("67890"), 8)
	AppendAPIResponseLog(ginCtx, []byte("ignored"), 8)

	data := MaterializeAPIResponse(ginCtx)
	if len(data) == 0 {
		t.Fatal("MaterializeAPIResponse() returned empty data")
	}
	if !bytes.Contains(data, []byte("12345")) {
		t.Fatalf("aggregated response missing first chunk: %q", data)
	}
	if bytes.Contains(data, []byte("ignored")) {
		t.Fatalf("aggregated response should not contain bytes beyond cap: %q", data)
	}
	if !bytes.Contains(data, []byte("[upstream response body truncated after 8 bytes]")) {
		t.Fatalf("aggregated response missing truncation marker: %q", data)
	}
	if bytes.Count(data, []byte("[upstream response body truncated after 8 bytes]")) != 1 {
		t.Fatalf("truncation marker count mismatch: %q", data)
	}
}

func TestTruncateLoggedPayload_TruncatesWithMarker(t *testing.T) {
	data := TruncateLoggedPayload([]byte("abcdefghij"), 8, "client request body")
	if !bytes.Contains(data, []byte("abcdefgh")) {
		t.Fatalf("truncated payload missing prefix: %q", data)
	}
	if bytes.Contains(data, []byte("abcdefghij")) {
		t.Fatalf("truncated payload should not contain full body: %q", data)
	}
	if !bytes.Contains(data, []byte("[client request body truncated after 8 bytes]")) {
		t.Fatalf("truncated payload missing marker: %q", data)
	}
}
