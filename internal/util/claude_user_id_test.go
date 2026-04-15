package util

import "testing"

func TestClaudeUserIDFromSeed_IsStableAndValid(t *testing.T) {
	first := ClaudeUserIDFromSeed("session-123")
	second := ClaudeUserIDFromSeed("session-123")
	if first != second {
		t.Fatalf("ClaudeUserIDFromSeed() unstable: %q != %q", first, second)
	}
	if !IsValidClaudeUserID(first) {
		t.Fatalf("ClaudeUserIDFromSeed() = %q, want valid Claude user id", first)
	}
}

func TestGenerateClaudeUserID_IsValid(t *testing.T) {
	got := GenerateClaudeUserID()
	if !IsValidClaudeUserID(got) {
		t.Fatalf("GenerateClaudeUserID() = %q, want valid Claude user id", got)
	}
}
