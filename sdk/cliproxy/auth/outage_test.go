package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerRuntimeOutageActiveUntilProbeRefresh(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	snapshotPath := filepath.Join(dir, "model-health.json")
	if err := os.WriteFile(snapshotPath, []byte(`{
  "updated_at": "2000-01-01T00:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "yunyi-codex",
      "base_url": "https://example.invalid",
      "models": [
        {"model": "gpt-5.3-codex", "status": "green", "tested_at": "2000-01-01T00:00:00Z"}
      ]
    }
  ]
}`), 0o644); err != nil {
		t.Fatalf("write initial snapshot: %v", err)
	}

	mgr := NewManager(nil, &FillFirstSelector{}, nil)
	mgr.SetUnifiedProbeSnapshotPath(snapshotPath)
	auth := &Auth{
		ID:       "a1",
		Provider: "codex",
		Prefix:   "yunyi-codex",
		Attributes: map[string]string{
			"base_url": "https://example.invalid",
		},
	}
	mgr.auths[auth.ID] = auth

	mgr.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.3-codex",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "bad gateway"},
	})

	scoreBefore := mgr.unifiedHealthForAuth(context.Background(), auth, "gpt-5.3-codex", mgr.authHealthSnapshot(auth.ID))
	if scoreBefore.Status != "unavailable" {
		t.Fatalf("status before probe refresh = %q, want unavailable", scoreBefore.Status)
	}

	if err := os.WriteFile(snapshotPath, []byte(`{
  "updated_at": "2099-01-01T00:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "yunyi-codex",
      "base_url": "https://example.invalid",
      "models": [
        {"model": "gpt-5.3-codex", "status": "green", "tested_at": "2099-01-01T00:00:00Z"}
      ]
    }
  ]
}`), 0o644); err != nil {
		t.Fatalf("write refreshed snapshot: %v", err)
	}
	mgr.SetUnifiedProbeSnapshotPath(snapshotPath)

	scoreAfter := mgr.unifiedHealthForAuth(context.Background(), auth, "gpt-5.3-codex", mgr.authHealthSnapshot(auth.ID))
	if scoreAfter.Status == "unavailable" {
		t.Fatalf("status after probe refresh = %q, want recovered from runtime outage", scoreAfter.Status)
	}

	items := mgr.RuntimeOutageSnapshot()
	if len(items) != 1 {
		t.Fatalf("RuntimeOutageSnapshot() len = %d, want 1", len(items))
	}
	if items[0].Prefix != "yunyi-codex" || items[0].Model != "gpt-5.3-codex" {
		t.Fatalf("RuntimeOutageSnapshot()[0] = %#v, want prefix/model recorded", items[0])
	}
}

func TestManagerRuntimeOutageRecordsSemanticRecoveredSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	snapshotPath := filepath.Join(dir, "model-health.json")
	if err := os.WriteFile(snapshotPath, []byte(`{
  "updated_at": "2000-01-01T00:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "covs",
      "base_url": "https://rsxermu666.cn/v1",
      "models": [
        {"model": "claude-opus-4-6", "status": "green", "tested_at": "2000-01-01T00:00:00Z"}
      ]
    }
  ]
}`), 0o644); err != nil {
		t.Fatalf("write initial snapshot: %v", err)
	}

	mgr := NewManager(nil, &FillFirstSelector{}, nil)
	mgr.SetUnifiedProbeSnapshotPath(snapshotPath)
	auth := &Auth{
		ID:       "covs-1",
		Provider: "claude",
		Prefix:   "covs",
		Attributes: map[string]string{
			"base_url": "https://rsxermu666.cn/v1",
		},
	}
	mgr.auths[auth.ID] = auth

	mgr.MarkResult(context.Background(), Result{
		AuthID:              auth.ID,
		Provider:            auth.Provider,
		Model:               "claude-opus-4-6",
		Success:             true,
		RuntimeOutageStatus: http.StatusBadGateway,
		RuntimeOutageReason: "covs_claude_pseudo_tool_stub",
	})

	score := mgr.unifiedHealthForAuth(context.Background(), auth, "claude-opus-4-6", mgr.authHealthSnapshot(auth.ID))
	if score.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", score.Status)
	}

	items := mgr.RuntimeOutageSnapshot()
	if len(items) != 1 {
		t.Fatalf("RuntimeOutageSnapshot() len = %d, want 1", len(items))
	}
	if items[0].Reason != "covs_claude_pseudo_tool_stub" {
		t.Fatalf("RuntimeOutageSnapshot()[0].Reason = %q, want %q", items[0].Reason, "covs_claude_pseudo_tool_stub")
	}
}

func TestManagerExecute_PropagatesReportedRuntimeOutageSignal(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, &FillFirstSelector{}, nil)
	mgr.RegisterExecutor(&contextReportingExecutor{})

	auth := &Auth{
		ID:       "covs-auth",
		Provider: "claude",
		Prefix:   "covs",
		Attributes: map[string]string{
			"base_url": "https://rsxermu666.cn/v1",
		},
	}
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: "claude-opus-4-6"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	if _, err := mgr.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: []byte(`{"model":"claude-opus-4-6"}`),
	}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	items := mgr.RuntimeOutageSnapshot()
	if len(items) != 1 {
		t.Fatalf("RuntimeOutageSnapshot() len = %d, want 1", len(items))
	}
	if items[0].Reason != "covs_claude_tool_semantics_failed" {
		t.Fatalf("RuntimeOutageSnapshot()[0].Reason = %q, want %q", items[0].Reason, "covs_claude_tool_semantics_failed")
	}
}

type contextReportingExecutor struct {
}

func (e *contextReportingExecutor) Identifier() string { return "claude" }

func (e *contextReportingExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ReportRuntimeOutageSignal(ctx, http.StatusBadGateway, "covs_claude_tool_semantics_failed")
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *contextReportingExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(ch)
	return ch, nil
}

func (e *contextReportingExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *contextReportingExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *contextReportingExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}
