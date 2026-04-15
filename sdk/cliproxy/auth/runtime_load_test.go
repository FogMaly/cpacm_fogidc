package auth

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestRuntimeLoadSnapshotTracksInflightAndWindows(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	auth := &Auth{ID: "a1", Provider: "codex"}
	mgr.auths[auth.ID] = auth

	finish := mgr.beginRuntimeLoad(auth, auth.Provider)
	if got := mgr.authRuntimeLoadSnapshot(auth.ID).Inflight; got != 1 {
		t.Fatalf("auth inflight = %d, want 1", got)
	}
	if got := mgr.providerRuntimeLoadSnapshot(auth.Provider).Inflight; got != 1 {
		t.Fatalf("provider inflight = %d, want 1", got)
	}

	finish(true, 120*time.Millisecond, nil)

	authSnapshot := mgr.authRuntimeLoadSnapshot(auth.ID)
	if authSnapshot.Inflight != 0 {
		t.Fatalf("auth inflight after finish = %d, want 0", authSnapshot.Inflight)
	}
	if authSnapshot.RecentRPS <= 0 {
		t.Fatalf("RecentRPS = %f, want > 0", authSnapshot.RecentRPS)
	}
	if authSnapshot.P95LatencyMS <= 0 {
		t.Fatalf("P95LatencyMS = %f, want > 0", authSnapshot.P95LatencyMS)
	}
}

func TestPickCandidateByAffinityMigratesWhenHardOverloaded(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ProviderConcurrency: map[string]int{"codex": 2},
			Scheduler: internalconfig.RoutingSchedulerConfig{
				SoftOverloadThreshold: 0.75,
				HardOverloadThreshold: 0.90,
			},
		},
	})

	target := &Auth{ID: "a1", Provider: "codex"}
	alt := &Auth{ID: "a2", Provider: "codex"}
	mgr.auths[target.ID] = target
	mgr.auths[alt.ID] = alt

	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"previous_response_id":"resp_123"}`),
	}
	key := buildRequestAffinityKey(affinityScopeForProvider("codex"), "gpt-5.3-codex", opts)
	mgr.requestAffinitySet(key, target.ID, time.Now().Add(time.Minute))

	finish := mgr.beginRuntimeLoad(target, target.Provider)
	defer finish(true, 10*time.Millisecond, nil)

	if got, ok := mgr.pickCandidateByAffinity(affinityScopeForProvider("codex"), "gpt-5.3-codex", opts, []*Auth{target, alt}); ok || got != nil {
		t.Fatalf("pickCandidateByAffinity() = %#v, %t, want nil,false when sticky target is hard overloaded", got, ok)
	}
}

func TestOrderProvidersByRuntimeScorePrefersLessLoadedProvider(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			ProviderConcurrency: map[string]int{
				"p1": 8,
				"p2": 8,
			},
		},
	})

	finishP1a := mgr.beginRuntimeLoad(&Auth{ID: "a1", Provider: "p1"}, "p1")
	finishP1b := mgr.beginRuntimeLoad(&Auth{ID: "a2", Provider: "p1"}, "p1")
	defer finishP1a(true, 10*time.Millisecond, nil)
	defer finishP1b(true, 10*time.Millisecond, nil)

	ordered := mgr.orderProvidersByRuntimeScore([]string{"p1", "p2"})
	if len(ordered) != 2 {
		t.Fatalf("len(ordered) = %d, want 2", len(ordered))
	}
	if ordered[0] != "p2" {
		t.Fatalf("ordered[0] = %q, want %q", ordered[0], "p2")
	}
}
