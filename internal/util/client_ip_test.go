package util

import (
	"net/http"
	"testing"
)

func TestResolveClientIP_PrefersPublicForwardedAddress(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.RemoteAddr = "10.0.0.15:42318"
	req.Header.Set("X-Real-IP", "10.0.0.15")
	req.Header.Set("X-Forwarded-For", "10.0.0.15, 198.51.100.24")

	if got := ResolveClientIP(req); got != "198.51.100.24" {
		t.Fatalf("ResolveClientIP() = %q, want %q", got, "198.51.100.24")
	}
}

func TestResolveClientIP_UsesForwardedHeader(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.RemoteAddr = "172.18.0.2:54321"
	req.Header.Set("Forwarded", `for=10.10.0.8;proto=https, for="[2001:db8::42]"`)

	if got := ResolveClientIP(req); got != "2001:db8::42" {
		t.Fatalf("ResolveClientIP() = %q, want %q", got, "2001:db8::42")
	}
}

func TestResolveClientIP_FallsBackToRemoteAddr(t *testing.T) {
	t.Parallel()

	req, err := http.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.RemoteAddr = "203.0.113.8:54432"

	if got := ResolveClientIP(req); got != "203.0.113.8" {
		t.Fatalf("ResolveClientIP() = %q, want %q", got, "203.0.113.8")
	}
}
