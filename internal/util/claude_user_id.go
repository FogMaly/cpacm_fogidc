package util

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var claudeUserIDPattern = regexp.MustCompile(`^user_[a-fA-F0-9]{64}_account__session_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsValidClaudeUserID reports whether the value matches Claude CLI style metadata.user_id.
func IsValidClaudeUserID(raw string) bool {
	return claudeUserIDPattern.MatchString(strings.TrimSpace(raw))
}

// GenerateClaudeUserID generates a random Claude CLI style metadata.user_id.
func GenerateClaudeUserID() string {
	hexBytes := make([]byte, 32)
	_, _ = rand.Read(hexBytes)
	return "user_" + hex.EncodeToString(hexBytes) + "_account__session_" + uuid.NewString()
}

// ClaudeUserIDFromSeed deterministically derives a Claude CLI style metadata.user_id from a stable seed.
func ClaudeUserIDFromSeed(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return GenerateClaudeUserID()
	}
	sum := sha256.Sum256([]byte(seed))
	sessionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
	return "user_" + hex.EncodeToString(sum[:]) + "_account__session_" + sessionID
}
