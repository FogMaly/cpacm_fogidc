package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

type routingTestExecutor struct {
	id       string
	mu       sync.Mutex
	calls    []string
	execFunc func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error)
}

func (e *routingTestExecutor) Identifier() string { return e.id }

func (e *routingTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls = append(e.calls, auth.ID+"|"+req.Model)
	e.mu.Unlock()
	if e.execFunc != nil {
		return e.execFunc(auth, req)
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *routingTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(ch)
	return ch, nil
}

func (e *routingTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *routingTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *routingTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}

func (e *routingTestExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.calls))
	copy(out, e.calls)
	return out
}

func newRoutingTestManager(t *testing.T, cfg *internalconfig.Config) *Manager {
	t.Helper()
	mgr := NewManager(nil, &FillFirstSelector{}, nil)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	mgr.SetConfig(cfg)
	return mgr
}

func registerRoutingTestAuth(t *testing.T, mgr *Manager, auth *Auth, models ...string) {
	t.Helper()
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth %s: %v", auth.ID, err)
	}
	modelInfos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		modelInfos = append(modelInfos, &registry.ModelInfo{ID: model})
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, modelInfos)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
}

func TestManagerExecuteV2_UsesProviderOffsetBeforeAuthSelection(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
	})
	execA := &routingTestExecutor{id: "p1"}
	execB := &routingTestExecutor{id: "p2"}
	mgr.RegisterExecutor(execA)
	mgr.RegisterExecutor(execB)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "b1", Provider: "p2"}, "m1")

	mgr.providerOffsets["m1"] = 1

	_, err := mgr.Execute(context.Background(), []string{"p1", "p2"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if calls := execB.Calls(); len(calls) != 1 || calls[0] != "b1|m1" {
		t.Fatalf("p2 calls = %v, want [b1|m1]", calls)
	}
	if calls := execA.Calls(); len(calls) != 0 {
		t.Fatalf("p1 calls = %v, want none", calls)
	}
	if got := mgr.providerOffsets["m1"]; got != 0 {
		t.Fatalf("providerOffsets[m1] = %d, want 0", got)
	}
}

func TestManagerExecuteV2_AdvancesProviderOffsetOnlyOnPrimaryAttempt(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
	})
	execA := &routingTestExecutor{id: "p1", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "boom"}
	}}
	execB := &routingTestExecutor{id: "p2"}
	mgr.RegisterExecutor(execA)
	mgr.RegisterExecutor(execB)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "b1", Provider: "p2"}, "m1")

	mgr.providerOffsets["m1"] = 0

	_, err := mgr.Execute(context.Background(), []string{"p1", "p2"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := mgr.providerOffsets["m1"]; got != 1 {
		t.Fatalf("providerOffsets[m1] = %d, want 1", got)
	}
}

func TestManagerExecuteV2_SwitchProjectFalseMovesToNextProvider(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: false},
	})
	execA := &routingTestExecutor{id: "p1", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		if auth.ID == "a1" {
			return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
		}
		return cliproxyexecutor.Response{Payload: []byte(`{"from":"a2"}`)}, nil
	}}
	execB := &routingTestExecutor{id: "p2"}
	mgr.RegisterExecutor(execA)
	mgr.RegisterExecutor(execB)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "a2", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "b1", Provider: "p2"}, "m1")

	_, err := mgr.Execute(context.Background(), []string{"p1", "p2"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := execA.Calls(); len(calls) != 1 || calls[0] != "a1|m1" {
		t.Fatalf("p1 calls = %v, want only first auth", calls)
	}
	if calls := execB.Calls(); len(calls) != 1 || calls[0] != "b1|m1" {
		t.Fatalf("p2 calls = %v, want fallback to p2", calls)
	}
}

func TestManagerExecuteV2_SwitchProjectTrueRetriesWithinProvider(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	execA := &routingTestExecutor{id: "p1", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		if auth.ID == "a1" {
			return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}
		}
		return cliproxyexecutor.Response{Payload: []byte(`{"from":"a2"}`)}, nil
	}}
	execB := &routingTestExecutor{id: "p2"}
	mgr.RegisterExecutor(execA)
	mgr.RegisterExecutor(execB)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "a2", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "b1", Provider: "p2"}, "m1")

	_, err := mgr.Execute(context.Background(), []string{"p1", "p2"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := execA.Calls(); len(calls) != 2 || calls[0] != "a1|m1" || calls[1] != "a2|m1" {
		t.Fatalf("p1 calls = %v, want [a1|m1 a2|m1]", calls)
	}
	if calls := execB.Calls(); len(calls) != 0 {
		t.Fatalf("p2 calls = %v, want none", calls)
	}
}

func TestManagerExecuteV2_ForeignPrefixedClaudeModelCanFallbackToOtherAuth(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	exec := &routingTestExecutor{id: "claude"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "yunyi-auth", Provider: "claude", Prefix: "yunyi-claude"}, "yunyi-claude/claude-sonnet-4-6")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "covs-auth", Provider: "claude", Prefix: "covs"}, "covs/claude-sonnet-4-6")

	_, err := mgr.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model: "yunyi-claude/claude-sonnet-4-6",
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "covs-auth|claude-sonnet-4-6" {
		t.Fatalf("calls = %v, want [covs-auth|claude-sonnet-4-6]", calls)
	}
}

func TestManagerExecuteV2_ForeignPrefixedClaudeModelSkipsExcludedFallbackAuth(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	exec := &routingTestExecutor{id: "claude"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "whitedream-auth",
		Provider: "claude",
		Prefix:   "whitedream-max",
		Attributes: map[string]string{
			"excluded_models": "*",
		},
	}, "whitedream-max/claude-sonnet-4-6")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "covs-auth",
		Provider: "claude",
		Prefix:   "covs",
	}, "covs/claude-sonnet-4-6")

	_, err := mgr.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model: "yunyi-claude/claude-sonnet-4-6",
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "covs-auth|claude-sonnet-4-6" {
		t.Fatalf("calls = %v, want [covs-auth|claude-sonnet-4-6]", calls)
	}
}

func TestManagerExecuteV2_SwitchPreviewModelUsesFallbackModel(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchPreviewModel: true},
	})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "c1", Provider: "codex"}, "gpt-5.3-codex")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5-codex"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "c1|gpt-5.3-codex" {
		t.Fatalf("calls = %v, want fallback model gpt-5.3-codex", calls)
	}
}

func TestManagerExecuteV2_CodexResponsesStickyAffinityKeepsSameAuth(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "round-robin"},
	})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native-2",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")

	optsConv1 := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: []byte(`{"prompt_cache_key":"conv-v2-1"}`),
	}
	optsConv2 := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: []byte(`{"prompt_cache_key":"conv-v2-2"}`),
	}

	for _, opts := range []cliproxyexecutor.Options{optsConv1, optsConv1, optsConv2} {
		if _, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, opts); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	}

	if calls := exec.Calls(); len(calls) != 3 || calls[0] != "z-native-1|gpt-5.4" || calls[1] != "z-native-1|gpt-5.4" || calls[2] != "z-native-2|gpt-5.4" {
		t.Fatalf("codex calls = %v, want [z-native-1|gpt-5.4 z-native-1|gpt-5.4 z-native-2|gpt-5.4]", calls)
	}
}

func TestManagerExecuteV2_StrictPublicRoutingPreventsPreviewFallback(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchPreviewModel: true},
	})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "c1", Provider: "codex"}, "gpt-5.3-codex")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5-codex"}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.StrictPublicModelRoutingMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatalf("Execute() error = nil, want non-nil when strict public routing blocks fallback")
	}
	if calls := exec.Calls(); len(calls) != 0 {
		t.Fatalf("calls = %v, want no upstream execution", calls)
	}
}

func TestManagerExecuteV1_StrictPublicRoutingAllowsAPIKeyAliasForCPAMCResolvedModel(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:  "now-key",
				BaseURL: "https://nowcoding.ai/v1",
				Prefix:  "nowcoding",
				Models: []internalconfig.CodexModel{
					{Name: "claude-sonnet-4-6", Alias: "gpt-5.4"},
				},
			},
		},
	})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "nowcoding-auth",
		Provider: "codex",
		Prefix:   "nowcoding",
		Attributes: map[string]string{
			"api_key":  "now-key",
			"base_url": "https://nowcoding.ai/v1",
		},
	}, "nowcoding/gpt-5.4")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "nowcoding/gpt-5.4"}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.StrictPublicModelRoutingMetadataKey: true,
			cliproxyexecutor.RequestedModelMetadataKey:           "gpt-5.4",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "nowcoding-auth|claude-sonnet-4-6" {
		t.Fatalf("calls = %v, want [nowcoding-auth|claude-sonnet-4-6]", calls)
	}
}

func TestManagerExecuteV1_DirectPrefixedModelStaysOnMatchingAuth(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:  "now-key",
				BaseURL: "https://nowcoding.ai/v1",
				Prefix:  "nowcoding",
				Models: []internalconfig.CodexModel{
					{Name: "claude-sonnet-4-6", Alias: "gpt-5.4"},
				},
			},
		},
	})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "other-auth-v1",
		Provider: "codex",
		Prefix:   "soapapi",
	}, "soapapi/gpt-5.4")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "nowcoding-auth-v1",
		Provider: "codex",
		Prefix:   "nowcoding",
		Attributes: map[string]string{
			"api_key":  "now-key",
			"base_url": "https://nowcoding.ai/v1",
		},
	}, "nowcoding/gpt-5.4")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "nowcoding/gpt-5.4"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "nowcoding-auth-v1|claude-sonnet-4-6" {
		t.Fatalf("calls = %v, want [nowcoding-auth-v1|claude-sonnet-4-6]", calls)
	}
}

func TestManagerExecuteV2_StrictPublicRoutingAllowsAPIKeyAliasForCPAMCResolvedModel(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:  "now-key",
				BaseURL: "https://nowcoding.ai/v1",
				Prefix:  "nowcoding",
				Models: []internalconfig.CodexModel{
					{Name: "claude-sonnet-4-6", Alias: "gpt-5.4"},
				},
			},
		},
	})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "nowcoding-auth-v2",
		Provider: "codex",
		Prefix:   "nowcoding",
		Attributes: map[string]string{
			"api_key":  "now-key",
			"base_url": "https://nowcoding.ai/v1",
		},
	}, "nowcoding/gpt-5.4")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "nowcoding/gpt-5.4"}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.StrictPublicModelRoutingMetadataKey: true,
			cliproxyexecutor.RequestedModelMetadataKey:           "gpt-5.4",
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "nowcoding-auth-v2|claude-sonnet-4-6" {
		t.Fatalf("calls = %v, want [nowcoding-auth-v2|claude-sonnet-4-6]", calls)
	}
}

func TestManagerExecuteV2_DirectPrefixedModelStaysOnMatchingAuth(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:  "now-key",
				BaseURL: "https://nowcoding.ai/v1",
				Prefix:  "nowcoding",
				Models: []internalconfig.CodexModel{
					{Name: "claude-sonnet-4-6", Alias: "gpt-5.4"},
				},
			},
		},
	})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "other-auth-v2",
		Provider: "codex",
		Prefix:   "soapapi",
	}, "soapapi/gpt-5.4")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "nowcoding-auth-v2-direct",
		Provider: "codex",
		Prefix:   "nowcoding",
		Attributes: map[string]string{
			"api_key":  "now-key",
			"base_url": "https://nowcoding.ai/v1",
		},
	}, "nowcoding/gpt-5.4")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "nowcoding/gpt-5.4"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "nowcoding-auth-v2-direct|claude-sonnet-4-6" {
		t.Fatalf("calls = %v, want [nowcoding-auth-v2-direct|claude-sonnet-4-6]", calls)
	}
}

func TestManagerExecuteShadow_RecordsPrimaryRouteDifference(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "shadow", Strategy: "round-robin"},
	})
	execA := &routingTestExecutor{id: "p1"}
	execB := &routingTestExecutor{id: "p2"}
	mgr.RegisterExecutor(execA)
	mgr.RegisterExecutor(execB)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "b1", Provider: "p2"}, "m1")

	mgr.providerOffsets["m1"] = 1

	_, err := mgr.Execute(context.Background(), []string{"p1", "p2"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	events := mgr.RecentRoutingEvents(10)
	found := false
	for _, event := range events {
		if event.ResultStatus != "shadow_compare" {
			continue
		}
		found = true
		if event.ChosenProvider != "p1" {
			t.Fatalf("ChosenProvider = %q, want %q", event.ChosenProvider, "p1")
		}
		if event.CompareProvider != "p2" {
			t.Fatalf("CompareProvider = %q, want %q", event.CompareProvider, "p2")
		}
		if event.CompareDiff == "match" {
			t.Fatalf("CompareDiff = match, want a route difference")
		}
	}
	if !found {
		t.Fatalf("expected shadow_compare event, got %#v", events)
	}
}

func TestManagerRoutingSnapshot_AggregatesProviderAndModelStatus(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
	})

	if _, err := mgr.Register(context.Background(), &Auth{
		ID:       "a1",
		Provider: "claude",
		ModelStates: map[string]*ModelState{
			"claude-sonnet-4": {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: time.Now().Add(2 * time.Minute),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: time.Now().Add(2 * time.Minute),
				},
			},
		},
	}); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if _, err := mgr.Register(context.Background(), &Auth{
		ID:       "a2",
		Provider: "claude",
	}); err != nil {
		t.Fatalf("register a2: %v", err)
	}
	if _, err := mgr.Register(context.Background(), &Auth{
		ID:       "g1",
		Provider: "gemini",
		Disabled: true,
	}); err != nil {
		t.Fatalf("register g1: %v", err)
	}

	mgr.providerOffsets["claude-sonnet-4"] = 1
	mgr.recordRoutingEvent(RoutingEvent{
		RequestID:      "r1",
		EngineVersion:  "v2",
		RequestedModel: "claude-sonnet-4",
		ResultStatus:   "success",
	})

	snapshot := mgr.RoutingSnapshot(10)
	if snapshot.Engine != "v2" {
		t.Fatalf("snapshot.Engine = %q, want %q", snapshot.Engine, "v2")
	}
	if snapshot.Strategy != "fill-first" {
		t.Fatalf("snapshot.Strategy = %q, want %q", snapshot.Strategy, "fill-first")
	}
	if snapshot.ProviderOffsets["claude-sonnet-4"] != 1 {
		t.Fatalf("provider offset = %d, want 1", snapshot.ProviderOffsets["claude-sonnet-4"])
	}
	if len(snapshot.RecentEvents) != 1 || snapshot.RecentEvents[0].RequestID != "r1" {
		t.Fatalf("RecentEvents = %#v, want one event r1", snapshot.RecentEvents)
	}
	if len(snapshot.StateOwnershipMatrix) < 3 {
		t.Fatalf("StateOwnershipMatrix = %#v, want at least 3 levels", snapshot.StateOwnershipMatrix)
	}
	if len(snapshot.ErrorActions) < 5 {
		t.Fatalf("ErrorActions = %#v, want catalog entries", snapshot.ErrorActions)
	}

	foundClaude := false
	foundGemini := false
	for _, provider := range snapshot.Providers {
		switch provider.Provider {
		case "claude":
			foundClaude = true
			if provider.TotalAuths != 2 || provider.AvailableAuths != 1 || provider.CooldownAuths != 1 {
				t.Fatalf("claude provider stats = %#v, want total=2 available=1 cooldown=1", provider)
			}
		case "gemini":
			foundGemini = true
			if provider.DisabledAuths != 1 {
				t.Fatalf("gemini provider stats = %#v, want disabled=1", provider)
			}
		}
	}
	if !foundClaude || !foundGemini {
		t.Fatalf("Providers = %#v, want claude and gemini", snapshot.Providers)
	}

	foundModel := false
	for _, model := range snapshot.Models {
		if model.Provider == "claude" && model.Model == "claude-sonnet-4" {
			foundModel = true
			if model.TotalAuths != 1 || model.CooldownAuths != 1 || model.QuotaExceededAuths != 1 {
				t.Fatalf("model stats = %#v, want total=1 cooldown=1 quota=1", model)
			}
		}
	}
	if !foundModel {
		t.Fatalf("Models = %#v, want claude-sonnet-4 entry", snapshot.Models)
	}
}

func TestManagerExecuteV2_InvalidRequestStopsRoutingImmediately(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
	})
	execA := &routingTestExecutor{id: "p1", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_request_error: bad payload"}
	}}
	execB := &routingTestExecutor{id: "p2"}
	mgr.RegisterExecutor(execA)
	mgr.RegisterExecutor(execB)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "b1", Provider: "p2"}, "m1")

	_, err := mgr.Execute(context.Background(), []string{"p1", "p2"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatalf("Execute() error = nil, want invalid request error")
	}
	if calls := execA.Calls(); len(calls) != 1 {
		t.Fatalf("p1 calls = %v, want 1", calls)
	}
	if calls := execB.Calls(); len(calls) != 0 {
		t.Fatalf("p2 calls = %v, want 0 because invalid request must stop routing", calls)
	}
}

func TestManagerExecuteV2_ProtocolMismatchFallsBackToNextProvider(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	execA := &routingTestExecutor{id: "p1", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusBadRequest, Message: `{"detail":"Unsupported content type"}`}
	}}
	execB := &routingTestExecutor{id: "p2", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{Payload: []byte(`{"from":"p2"}`)}, nil
	}}
	mgr.RegisterExecutor(execA)
	mgr.RegisterExecutor(execB)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "a2", Provider: "p1"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "b1", Provider: "p2"}, "m1")

	resp, err := mgr.Execute(context.Background(), []string{"p1", "p2"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(resp.Payload) != `{"from":"p2"}` {
		t.Fatalf("response payload = %s, want p2 fallback", string(resp.Payload))
	}
	if calls := execA.Calls(); len(calls) != 1 || calls[0] != "a1|m1" {
		t.Fatalf("p1 calls = %v, want only first auth before provider fallback", calls)
	}
	if calls := execB.Calls(); len(calls) != 1 || calls[0] != "b1|m1" {
		t.Fatalf("p2 calls = %v, want [b1|m1]", calls)
	}
}

func TestManagerExecuteV2_UnauthorizedBlocksAuthAndTriesNextAuth(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	exec := &routingTestExecutor{id: "claude", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		if auth.ID == "a1" {
			return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired token"}
		}
		return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
	}}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "claude"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "a2", Provider: "claude"}, "m1")

	_, err := mgr.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 2 || calls[0] != "a1|m1" || calls[1] != "a2|m1" {
		t.Fatalf("calls = %v, want [a1|m1 a2|m1]", calls)
	}
}

func TestManagerExecuteV2_ModelNotFoundRetriesWithinProviderWhenProjectSwitchEnabled(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	exec := &routingTestExecutor{id: "codex", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		if auth.ID == "a1" {
			return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotFound, Message: "model missing"}
		}
		return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
	}}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "codex"}, "m1")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "a2", Provider: "codex"}, "m1")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 2 || calls[0] != "a1|m1" || calls[1] != "a2|m1" {
		t.Fatalf("calls = %v, want [a1|m1 a2|m1]", calls)
	}
}

func TestManagerExecuteV2_DoesNotRepeatSameRouteWithinSingleRequest(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	exec := &routingTestExecutor{id: "claude", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "transient"}
	}}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "a1", Provider: "claude"}, "m1")

	_, err := mgr.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "m1"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatalf("Execute() error = nil, want transient error")
	}
	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "a1|m1" {
		t.Fatalf("calls = %v, want single attempt on same route", calls)
	}
}

func TestManagerExecuteV2_TransportResetCooldownSkipsRecentlyFailedClaudeAuths(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	exec := &routingTestExecutor{id: "claude", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		if strings.HasPrefix(auth.ID, "bad-") {
			return cliproxyexecutor.Response{}, errors.New(`Post "https://rsxermu666.cn/v1/messages": write tcp4 10.0.0.1:12345->104.21.72.249:443: write: connection reset by peer`)
		}
		return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
	}}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "bad-1", Provider: "claude", Prefix: "yunyi-claude"}, "claude-sonnet-4-6")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "bad-2", Provider: "claude", Prefix: "covs"}, "claude-sonnet-4-6")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "bad-3", Provider: "claude", Prefix: "heci"}, "claude-sonnet-4-6")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "good-1", Provider: "claude", Prefix: "good"}, "claude-sonnet-4-6")

	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6"}
	if _, err := mgr.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}

	calls := exec.Calls()
	if got, want := len(calls), 4; got != want {
		t.Fatalf("first calls len = %d, want %d (%v)", got, want, calls)
	}

	if _, err := mgr.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	calls = exec.Calls()
	if got, want := len(calls), 5; got != want {
		t.Fatalf("total calls len = %d, want %d (%v)", got, want, calls)
	}
	if got := calls[4]; got != "good-1|claude-sonnet-4-6" {
		t.Fatalf("second request selected %q, want only good-1|claude-sonnet-4-6", got)
	}
}

func TestManagerExecuteV2_TransportResetCooldownReturns503WhileAllClaudeAuthsCooling(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	mgr := newRoutingTestManager(t, &internalconfig.Config{
		Routing:       internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
		QuotaExceeded: internalconfig.QuotaExceeded{SwitchProject: true},
	})
	exec := &routingTestExecutor{id: "claude", execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, errors.New(`Post "https://rsxermu666.cn/v1/messages": write tcp4 10.0.0.1:12345->104.21.72.249:443: write: connection reset by peer`)
	}}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "bad-1", Provider: "claude", Prefix: "yunyi-claude"}, "claude-sonnet-4-6")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "bad-2", Provider: "claude", Prefix: "covs"}, "claude-sonnet-4-6")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "bad-3", Provider: "claude", Prefix: "heci"}, "claude-sonnet-4-6")

	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6"}
	if _, err := mgr.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("first Execute() error = nil, want transport failure")
	}

	_, err := mgr.Execute(context.Background(), []string{"claude"}, req, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("second Execute() error = nil, want service unavailable while auths cool down")
	}
	authErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("second Execute() error type = %T, want *Error", err)
	}
	if got := authErr.StatusCode(); got != http.StatusServiceUnavailable {
		t.Fatalf("second Execute() status = %d, want %d", got, http.StatusServiceUnavailable)
	}

	if calls := exec.Calls(); len(calls) != 3 {
		t.Fatalf("calls = %v, want only first request to hit all three auths", calls)
	}
}
