package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestBuildRequestAffinityKey_UsesSyntheticMetadataSeed(t *testing.T) {
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SessionAffinitySeedMetadataKey: "fallback:client-1",
		},
	}

	key := buildRequestAffinityKey(affinityScopeForProvider("claude"), "claude-opus-4-6", opts)
	if key == "" {
		t.Fatal("buildRequestAffinityKey() = empty, want non-empty")
	}
	if got := buildRequestAffinityKey(affinityScopeForProvider("claude"), "claude-opus-4-6", opts); got != key {
		t.Fatalf("buildRequestAffinityKey() not stable: first=%q second=%q", key, got)
	}
}

func TestPickCandidateByAffinity_UsesHeaderSessionID(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})

	target := &Auth{ID: "a1", Provider: "claude"}
	alt := &Auth{ID: "a2", Provider: "claude"}
	mgr.auths[target.ID] = target
	mgr.auths[alt.ID] = alt

	opts := cliproxyexecutor.Options{
		Headers: http.Header{"Session-Id": []string{"sess-123"}},
	}
	key := buildRequestAffinityKey(affinityScopeForProvider("claude"), "claude-opus-4-6", opts)
	mgr.requestAffinitySet(key, target.ID, time.Now().Add(time.Minute))

	got, ok := mgr.pickCandidateByAffinity(affinityScopeForProvider("claude"), "claude-opus-4-6", opts, []*Auth{target, alt})
	if !ok || got == nil || got.ID != "a1" {
		t.Fatalf("pickCandidateByAffinity() = %#v, %t, want a1,true", got, ok)
	}
}

func TestBuildRequestAffinityKey_UsesClaudeMetadataUserIDSession(t *testing.T) {
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{
			"model":"claude-opus-4-6",
			"metadata":{"user_id":"user_04408135829d8e3beb46401215e3163fab6c5f9c857b575056e37708c9e3f91b_account__session_b77671ff-818b-4676-9aee-b6d6466cbd6c"}
		}`),
	}

	key := buildRequestAffinityKey(affinityScopeForProvider("claude"), "claude-opus-4-6", opts)
	if key == "" {
		t.Fatal("buildRequestAffinityKey() = empty, want non-empty")
	}
	if !strings.Contains(key, "metadata.user_id.account__session") {
		t.Fatalf("buildRequestAffinityKey() = %q, want metadata.user_id.account__session seed source", key)
	}
}

func TestManagerExecuteV2_SyntheticSessionAffinityKeepsSameAuth(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "round-robin"},
	})
	exec := &routingTestExecutor{id: "claude"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{ID: "claude-1", Provider: "claude"}, "claude-opus-4-6")
	registerRoutingTestAuth(t, mgr, &Auth{ID: "claude-2", Provider: "claude"}, "claude-opus-4-6")

	optsA := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SessionAffinitySeedMetadataKey: "fallback:user-a",
		},
	}
	optsB := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SessionAffinitySeedMetadataKey: "fallback:user-b",
		},
	}

	for _, opts := range []cliproxyexecutor.Options{optsA, optsA, optsB} {
		if _, err := mgr.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-4-6"}, opts); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	}

	if calls := exec.Calls(); len(calls) != 3 || calls[0] != "claude-1|claude-opus-4-6" || calls[1] != "claude-1|claude-opus-4-6" || calls[2] != "claude-2|claude-opus-4-6" {
		t.Fatalf("claude calls = %v, want [claude-1|claude-opus-4-6 claude-1|claude-opus-4-6 claude-2|claude-opus-4-6]", calls)
	}
}
