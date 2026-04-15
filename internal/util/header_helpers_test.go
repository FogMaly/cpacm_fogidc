package util

import (
	"net/http"
	"testing"
)

func TestApplyCustomHeadersFromAttrs_SkipsReservedInternalHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	ApplyCustomHeadersFromAttrs(req, map[string]string{
		"header:X-NewAPI-Username":   "test-user",
		"header:X-NewAPI-Password":   "test-password",
		"header:X-NewAPI-Token-Name": "test-codex-plan",
		"header:X-Custom-Test":       "ok",
	})

	if got := req.Header.Get("X-NewAPI-Username"); got != "" {
		t.Fatalf("X-NewAPI-Username = %q, want empty", got)
	}
	if got := req.Header.Get("X-NewAPI-Password"); got != "" {
		t.Fatalf("X-NewAPI-Password = %q, want empty", got)
	}
	if got := req.Header.Get("X-NewAPI-Token-Name"); got != "" {
		t.Fatalf("X-NewAPI-Token-Name = %q, want empty", got)
	}
	if got := req.Header.Get("X-Custom-Test"); got != "ok" {
		t.Fatalf("X-Custom-Test = %q, want ok", got)
	}
}

func TestApplyReservedProviderHeadersFromAttrs_AppliesNewAPIHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	ApplyReservedProviderHeadersFromAttrs(req, map[string]string{
		"header:X-NewAPI-Username": "test-user",
		"header:X-NewAPI-Password": "test-password",
		"header:X-Custom-Test":     "ok",
	})

	if got := req.Header.Get("X-NewAPI-Username"); got != "test-user" {
		t.Fatalf("X-NewAPI-Username = %q, want test-user", got)
	}
	if got := req.Header.Get("X-NewAPI-Password"); got != "test-password" {
		t.Fatalf("X-NewAPI-Password = %q, want test-password", got)
	}
	if got := req.Header.Get("X-Custom-Test"); got != "" {
		t.Fatalf("X-Custom-Test = %q, want empty", got)
	}
}

func TestHeaderValueFromAttrsCI_MatchesConfiguredHeaderCaseInsensitively(t *testing.T) {
	got := HeaderValueFromAttrsCI(map[string]string{
		"header:X-NewAPI-User-Id": "user-123",
	}, "x-newapi-user-id", "x-new-api-user-id")
	if got != "user-123" {
		t.Fatalf("HeaderValueFromAttrsCI() = %q, want user-123", got)
	}
}
