package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestManager_ShouldRetryAfterError_RespectsAuthRequestRetryOverride(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"request_retry": float64(0),
		},
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for request_retry=0, got true (wait=%v)", wait)
	}

	auth.Metadata["request_retry"] = float64(1)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	wait, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true for request_retry=1, got false")
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}

	_, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 1, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false on attempt=1 for request_retry=1, got true")
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model"
	m.MarkResult(context.Background(), Result{
		AuthID:   "auth-1",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 500, Message: "boom"},
	})

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestStatusCodeFromError_InfersGatewayTimeoutFromTransportTimeout(t *testing.T) {
	err := errors.New(`Post "https://cdn1.yunyi.cfd/claude/v1/messages": http2: timeout awaiting response headers`)
	if got := statusCodeFromError(err); got != http.StatusGatewayTimeout {
		t.Fatalf("statusCodeFromError = %d, want %d", got, http.StatusGatewayTimeout)
	}
}

func TestStatusCodeFromError_InfersBadGatewayFromTransportReset(t *testing.T) {
	err := errors.New(`Post "https://rsxermu666.cn/v1/messages": write tcp4 10.0.0.1:12345->104.21.72.249:443: write: connection reset by peer`)
	if got := statusCodeFromError(err); got != http.StatusBadGateway {
		t.Fatalf("statusCodeFromError = %d, want %d", got, http.StatusBadGateway)
	}
}

func TestStatusCodeFromError_TreatsModelNotSupportedAsNotFound(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"type":"error","error":{"type":"model_not_supported","message":"Model 'claude-opus-4-6' is not supported."}}`,
	}
	if got := statusCodeFromError(err); got != http.StatusNotFound {
		t.Fatalf("statusCodeFromError = %d, want %d", got, http.StatusNotFound)
	}
}

func TestIsRequestInvalidError_FalseForModelNotSupported(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"type":"error","error":{"type":"model_not_supported","message":"Model 'claude-opus-4-6' is not supported."}}`,
	}
	if isRequestInvalidError(err) {
		t.Fatal("expected model_not_supported to remain retryable across auths")
	}
}

func TestManager_MarkResult_CoolsDownTransportTimeoutWithoutHTTPStatus(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-timeout",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-4-6"
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			Message: `Post "https://cdn1.yunyi.cfd/claude/v1/messages": http2: timeout awaiting response headers`,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected transport timeout to trigger cooldown")
	}
	if !state.NextRetryAfter.After(time.Now()) {
		t.Fatalf("expected NextRetryAfter in the future, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_ModelNotSupportedSuspendsModel(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-model-not-supported",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-4-6"
	now := time.Now()
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusBadRequest,
			Message:    `{"type":"error","error":{"type":"model_not_supported","message":"Model 'claude-opus-4-6' is not supported."}}`,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected model_not_supported to suspend the model")
	}
	if state.NextRetryAfter.Sub(now) < 11*time.Hour {
		t.Fatalf("expected long suspension window, got %v", state.NextRetryAfter.Sub(now))
	}
}

func TestManager_MarkResult_TransportTimeoutStillBacksOffWhenCoolingDisabled(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-timeout-no-cooling",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-4-6"
	now := time.Now()
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			Message: `Post "https://cdn1.yunyi.cfd/claude/v1/messages": http2: timeout awaiting response headers`,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected transport timeout to trigger short cooldown even when disable-cooling=true")
	}
	if !state.NextRetryAfter.After(now) {
		t.Fatalf("expected NextRetryAfter in the future, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_TransportResetStillBacksOffWhenCoolingDisabled(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-reset-no-cooling",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-4-6"
	now := time.Now()
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			Message: `Post "https://rsxermu666.cn/v1/messages": write tcp4 10.0.0.1:12345->104.21.72.249:443: write: connection reset by peer`,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected transport reset to trigger short cooldown even when disable-cooling=true")
	}
	if !state.NextRetryAfter.After(now) {
		t.Fatalf("expected NextRetryAfter in the future, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_ShortContinuationStillBacksOffWhenCoolingDisabled(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-short-continuation-no-cooling",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-4-6"
	now := time.Now()
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusBadGateway,
			Message:    `claude executor: rejected short continuation response (output_tokens=20 text="好的，开始全速写代码。先搞定项目骨架的所有核心文件。")`,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected short continuation rejection to trigger short cooldown even when disable-cooling=true")
	}
	if !state.NextRetryAfter.After(now) {
		t.Fatalf("expected NextRetryAfter in the future, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_EmptyVisibleContentStillBacksOffWhenCoolingDisabled(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-empty-visible-content-no-cooling",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-sonnet-4-6"
	now := time.Now()
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusBadGateway,
			Message:    "covs upstream returned empty visible content after fallback retry",
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected empty visible content failure to trigger short cooldown even when disable-cooling=true")
	}
	if !state.NextRetryAfter.After(now) {
		t.Fatalf("expected NextRetryAfter in the future, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_PseudoToolStubStillBacksOffWhenCoolingDisabled(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-pseudo-tool-stub-no-cooling",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-4-6"
	now := time.Now()
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusBadGateway,
			Message:    `claude executor: rejected pseudo tool stub response (text="<claude:tool_call>\n\n</claude:tool_call>")`,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected pseudo tool stub rejection to trigger short cooldown even when disable-cooling=true")
	}
	if !state.NextRetryAfter.After(now) {
		t.Fatalf("expected NextRetryAfter in the future, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_ForeignAssistantIdentityStillBacksOffWhenCoolingDisabled(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-foreign-identity-no-cooling",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-opus-4-6"
	now := time.Now()
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusBadGateway,
			Message:    `claude executor: rejected foreign assistant identity response (text="你好！我是 Cursor，由 Anysphere 开发的 AI 助手。")`,
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected foreign assistant identity rejection to trigger short cooldown even when disable-cooling=true")
	}
	if !state.NextRetryAfter.After(now) {
		t.Fatalf("expected NextRetryAfter in the future, got %v", state.NextRetryAfter)
	}
}

func TestManager_MarkResult_IncompleteClaudeStreamStillBacksOffWhenCoolingDisabled(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-incomplete-stream-no-cooling",
		Provider: "claude",
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "claude-sonnet-4-6"
	now := time.Now()
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusRequestTimeout,
			Message:    "stream disconnected before completion: stream closed before message_stop",
		},
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected incomplete Claude stream to trigger short cooldown even when disable-cooling=true")
	}
	if !state.NextRetryAfter.After(now) {
		t.Fatalf("expected NextRetryAfter in the future, got %v", state.NextRetryAfter)
	}
}
