package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestManagerRoutingStrategy_RecognizesHealthAware(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Engine:   "v2",
			Strategy: "health-aware",
		},
	})

	if got := mgr.routingEngine(); got != "v2" {
		t.Fatalf("routingEngine() = %q, want %q", got, "v2")
	}
	if got := mgr.routingStrategy(); got != "health-aware" {
		t.Fatalf("routingStrategy() = %q, want %q", got, "health-aware")
	}
	if got := mgr.currentRoutingEngineSelectorLabel(); got != "health-aware" {
		t.Fatalf("currentRoutingEngineSelectorLabel() = %q, want %q", got, "health-aware")
	}
}
