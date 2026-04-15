package claude

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestForwardClaudeStream_WritesTerminalErrorEventAfterPartialPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/api/provider/anthropic/v1/messages?beta=true", nil)
	c.Header("Content-Type", "text/event-stream")
	_, _ = rec.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"},\"index\":0}\n\n"))

	handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	close(data)
	errs <- &interfaces.ErrorMessage{
		StatusCode: 408,
		Error:      errors.New("stream disconnected before completion: stream closed before message_stop"),
	}
	close(errs)

	handler.forwardClaudeStream(c, rec, func(error) {}, data, errs)

	body := rec.Body.String()
	if !strings.Contains(body, `"type":"text_delta","text":"OK"`) {
		t.Fatalf("body = %q, want streamed payload", body)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("body = %q, want terminal error event", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("body = %q, want message_stop detail in error", body)
	}
}
