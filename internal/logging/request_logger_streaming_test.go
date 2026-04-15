package logging

import (
	"os"
	"strings"
	"testing"
)

func TestFileStreamingLogWriter_TruncatesResponseBodyAtConfiguredLimit(t *testing.T) {
	t.Parallel()

	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	logger.SetStreamBodyLimit(8)

	writerAny, err := logger.LogStreamingRequest(
		"/v1/responses",
		"POST",
		map[string][]string{"Content-Type": {"application/json"}},
		[]byte(`{"stream":true}`),
		"stream-limit-test",
	)
	if err != nil {
		t.Fatalf("LogStreamingRequest() error = %v", err)
	}

	writer, ok := writerAny.(*FileStreamingLogWriter)
	if !ok {
		t.Fatalf("LogStreamingRequest() type = %T, want *FileStreamingLogWriter", writerAny)
	}

	if err := writer.WriteStatus(200, map[string][]string{"Content-Type": {"text/event-stream"}}); err != nil {
		t.Fatalf("WriteStatus() error = %v", err)
	}

	writer.WriteChunkAsync([]byte("abcdefgh"))
	writer.WriteChunkAsync([]byte("TAILMARK"))
	writer.WriteChunkAsync([]byte("SHOULD_NOT_APPEAR"))

	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(writer.logFilePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "abcdefgh") {
		t.Fatalf("log missing retained prefix: %q", content)
	}
	if strings.Contains(content, "TAILMARK") {
		t.Fatalf("log should not contain truncated tail: %q", content)
	}
	if strings.Contains(content, "SHOULD_NOT_APPEAR") {
		t.Fatalf("log should ignore chunks after truncation: %q", content)
	}

	const marker = "[stream log truncated after 8 bytes]"
	if count := strings.Count(content, marker); count != 1 {
		t.Fatalf("truncation marker count = %d, want 1; content = %q", count, content)
	}
}
