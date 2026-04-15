package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestedModelForExecution_UsesCPAMSRequestedModel(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("cpams_requested_model", "gpt-5.4")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	if got := requestedModelForExecution(ctx, "nowcoding/gpt-5.4"); got != "gpt-5.4" {
		t.Fatalf("requestedModelForExecution() = %q, want %q", got, "gpt-5.4")
	}
}
