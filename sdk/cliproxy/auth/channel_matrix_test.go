package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerExecute_ChannelMatrix_UsesEnabledAuthOnly(t *testing.T) {
	testCases := []struct {
		name     string
		provider string
		model    string
	}{
		{name: "gemini", provider: "gemini", model: "gemini-2.5-pro"},
		{name: "claude", provider: "claude", model: "claude-sonnet-4-5"},
		{name: "codex", provider: "codex", model: "gpt-5.4"},
		{name: "compat", provider: "compat", model: "compat/gpt-4.1"},
		{name: "vertex", provider: "vertex", model: "gemini-2.5-pro"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newRoutingTestManager(t, &internalconfig.Config{
				Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
			})
			exec := &routingTestExecutor{id: tc.provider}
			mgr.RegisterExecutor(exec)

			registerRoutingTestAuth(t, mgr, &Auth{
				ID:       tc.provider + "-disabled-flag",
				Provider: tc.provider,
				Disabled: true,
				Status:   StatusActive,
			}, tc.model)
			registerRoutingTestAuth(t, mgr, &Auth{
				ID:       tc.provider + "-disabled-status",
				Provider: tc.provider,
				Status:   StatusDisabled,
			}, tc.model)
			registerRoutingTestAuth(t, mgr, &Auth{
				ID:       tc.provider + "-active",
				Provider: tc.provider,
				Status:   StatusActive,
			}, tc.model)

			_, err := mgr.Execute(context.Background(), []string{tc.provider}, cliproxyexecutor.Request{
				Model: tc.model,
			}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			if calls := exec.Calls(); len(calls) != 1 || calls[0] != tc.provider+"-active|"+tc.model {
				t.Fatalf("calls = %v, want [%s-active|%s]", calls, tc.provider, tc.model)
			}
		})
	}
}

func TestManagerExecute_ChannelMatrix_SkipsDisabledProvidersInMixedRouting(t *testing.T) {
	testCases := []struct {
		name             string
		disabledProvider string
		enabledProvider  string
		model            string
	}{
		{name: "gemini_to_claude", disabledProvider: "gemini", enabledProvider: "claude", model: "shared-model"},
		{name: "claude_to_codex", disabledProvider: "claude", enabledProvider: "codex", model: "shared-model"},
		{name: "codex_to_compat", disabledProvider: "codex", enabledProvider: "compat", model: "shared-model"},
		{name: "compat_to_vertex", disabledProvider: "compat", enabledProvider: "vertex", model: "shared-model"},
		{name: "vertex_to_gemini", disabledProvider: "vertex", enabledProvider: "gemini", model: "shared-model"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newRoutingTestManager(t, &internalconfig.Config{
				Routing: internalconfig.RoutingConfig{Engine: "v2", Strategy: "fill-first"},
			})

			disabledExec := &routingTestExecutor{id: tc.disabledProvider}
			enabledExec := &routingTestExecutor{id: tc.enabledProvider}
			mgr.RegisterExecutor(disabledExec)
			mgr.RegisterExecutor(enabledExec)

			registerRoutingTestAuth(t, mgr, &Auth{
				ID:       tc.disabledProvider + "-disabled",
				Provider: tc.disabledProvider,
				Disabled: true,
				Status:   StatusActive,
			}, tc.model)
			registerRoutingTestAuth(t, mgr, &Auth{
				ID:       tc.enabledProvider + "-active",
				Provider: tc.enabledProvider,
				Status:   StatusActive,
			}, tc.model)

			_, err := mgr.Execute(context.Background(), []string{tc.disabledProvider, tc.enabledProvider}, cliproxyexecutor.Request{
				Model: tc.model,
			}, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			if calls := disabledExec.Calls(); len(calls) != 0 {
				t.Fatalf("%s calls = %v, want none", tc.disabledProvider, calls)
			}
			if calls := enabledExec.Calls(); len(calls) != 1 || calls[0] != tc.enabledProvider+"-active|"+tc.model {
				t.Fatalf("%s calls = %v, want [%s-active|%s]", tc.enabledProvider, calls, tc.enabledProvider, tc.model)
			}
		})
	}
}
