package openai

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWriteOpenAISSEChunk_PreservesPrefixedData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	writeOpenAISSEChunk(c, []byte(`data: {"id":"x","object":"chat.completion.chunk"}`))

	if got := rec.Body.String(); strings.Count(got, "data: ") != 1 {
		t.Fatalf("body = %q, want single data prefix", got)
	}
}

func TestWriteOpenAISSEChunk_WrapsBareJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	writeOpenAISSEChunk(c, []byte(`{"id":"x","object":"chat.completion.chunk"}`))

	if got := rec.Body.String(); got != "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\"}\n\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteOpenAISSEChunk_NormalizesDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	writeOpenAISSEChunk(c, []byte("data: [DONE]"))

	if got := rec.Body.String(); got != "data: [DONE]\n\n" {
		t.Fatalf("body = %q", got)
	}
}
