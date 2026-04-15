package executor

import "testing"

func TestNormalizeOpenAICompatibleBaseURL_AppendsV1ForSiteRoot(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare host", in: "https://gmncode.cn", want: "https://gmncode.cn/v1"},
		{name: "host slash", in: "https://gmncode.cn/", want: "https://gmncode.cn/v1"},
		{name: "existing v1", in: "https://example.com/v1", want: "https://example.com/v1"},
		{name: "nested api path", in: "https://cdn1.yunyi.cfd/codex/v1", want: "https://cdn1.yunyi.cfd/codex/v1"},
		{name: "backend api path", in: "https://chatgpt.com/backend-api/codex", want: "https://chatgpt.com/backend-api/codex"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeOpenAICompatibleBaseURL(tc.in); got != tc.want {
				t.Fatalf("normalizeOpenAICompatibleBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildClaudeMessagesEndpoint_NormalizesCommonBases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare host", in: "https://api.anthropic.com", want: "https://api.anthropic.com/v1/messages"},
		{name: "v1 root", in: "https://nowcoding.ai/v1", want: "https://nowcoding.ai/v1/messages"},
		{name: "nested provider root", in: "https://cdn1.yunyi.cfd/claude", want: "https://cdn1.yunyi.cfd/claude/v1/messages"},
		{name: "existing messages", in: "https://example.com/v1/messages", want: "https://example.com/v1/messages"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildClaudeMessagesEndpoint(tc.in); got != tc.want {
				t.Fatalf("buildClaudeMessagesEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildClaudeMessagesCountTokensEndpoint(t *testing.T) {
	got := buildClaudeMessagesCountTokensEndpoint("https://nowcoding.ai/v1")
	want := "https://nowcoding.ai/v1/messages/count_tokens"
	if got != want {
		t.Fatalf("buildClaudeMessagesCountTokensEndpoint() = %q, want %q", got, want)
	}
}

func TestBuildClaudeMessagesRequestURL_OnlyOfficialAnthropicUsesBetaQuery(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "official anthropic", in: "https://api.anthropic.com", want: "https://api.anthropic.com/v1/messages?beta=true"},
		{name: "third party relay", in: "https://cdn1.yunyi.cfd/claude", want: "https://cdn1.yunyi.cfd/claude/v1/messages"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildClaudeMessagesRequestURL(tc.in); got != tc.want {
				t.Fatalf("buildClaudeMessagesRequestURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildClaudeMessagesCountTokensRequestURL_OnlyOfficialAnthropicUsesBetaQuery(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "official anthropic", in: "https://api.anthropic.com", want: "https://api.anthropic.com/v1/messages/count_tokens?beta=true"},
		{name: "third party relay", in: "https://cdn1.yunyi.cfd/claude", want: "https://cdn1.yunyi.cfd/claude/v1/messages/count_tokens"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildClaudeMessagesCountTokensRequestURL(tc.in); got != tc.want {
				t.Fatalf("buildClaudeMessagesCountTokensRequestURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestShouldUseClaudeThirdPartyRelayProfile(t *testing.T) {
	if !shouldUseClaudeThirdPartyRelayProfile("https://cdn1.yunyi.cfd/claude") {
		t.Fatal("expected third-party relay profile for yunyi Claude base URL")
	}
	if shouldUseClaudeThirdPartyRelayProfile("https://api.anthropic.com") {
		t.Fatal("did not expect third-party relay profile for official Anthropic base URL")
	}
}
