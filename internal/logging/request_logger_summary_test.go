package logging

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWriteNonStreamingLog_AppendsSummaryForCompletedToolCall(t *testing.T) {
	t.Parallel()

	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	var buf bytes.Buffer

	requestHeaders := map[string][]string{
		"User-Agent":   {"claude-cli/2.1.72 (external, cli)"},
		"Content-Type": {"application/json"},
	}
	responseHeaders := map[string][]string{
		"Content-Type":            {"text/event-stream"},
		"X-Cpams-Channel":         {"claude"},
		"X-Cpams-Decision":        {"reuse_sticky_selection"},
		"X-Cpams-Requested-Model": {"claude-opus-4-6(xhigh)"},
		"X-Cpams-Model":           {"covs/claude-opus-4-6(xhigh)"},
	}

	err := logger.writeNonStreamingLog(
		&buf,
		"/v1/chat/completions",
		"POST",
		requestHeaders,
		[]byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		"",
		[]byte("=== API REQUEST ===\nBody:\n{\"stream\":true}\n"),
		[]byte("=== API RESPONSE 1 ===\nStatus: 200\n"),
		nil,
		200,
		responseHeaders,
		[]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"Write\"}}]}}]}\n\nevent: message_stop\n\ndata: [DONE]\n"),
		nil,
		time.Unix(0, 0),
		time.Unix(1, 0),
	)
	if err != nil {
		t.Fatalf("writeNonStreamingLog() error = %v", err)
	}

	content := buf.String()
	for _, needle := range []string{
		"=== SUMMARY ===",
		"request_received: yes",
		"client_user_agent: claude-cli/2.1.72 (external, cli)",
		"streaming_response: yes",
		"tool_call_seen: yes",
		"first_tool_name: Write",
		"message_stop_seen: yes",
		"done_seen: yes",
		"reply_tail_complete: yes",
		"upstream_attempts: 1",
		"upstream_status_chain: 200",
		"transport_error: none",
		"final_status: 200",
		"cpams_channel: claude",
		"cpams_decision: reuse_sticky_selection",
	} {
		if !strings.Contains(content, needle) {
			t.Fatalf("log missing %q\nfull log:\n%s", needle, content)
		}
	}
}

func TestBuildRequestLogSummary_CapturesFailureChain(t *testing.T) {
	t.Parallel()

	summary := buildRequestLogSummary(
		map[string][]string{"User-Agent": {"claude-cli/2.1.72"}},
		[]byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		nil,
		[]byte("=== API RESPONSE 1 ===\nStatus: 500\n\n=== API RESPONSE 2 ===\nStatus: 401\n\n=== API RESPONSE 3 ===\nError: Post \"https://example.com/v1/messages\": write: connection reset by peer\n"),
		nil,
		500,
		map[string][]string{
			"Content-Type":            {"application/json"},
			"X-Cpams-Channel":         {"claude"},
			"X-Cpams-Decision":        {"issue_new_token_health_aware_same_model"},
			"X-Cpams-Requested-Model": {"claude-opus-4-6(xhigh)"},
			"X-Cpams-Model":           {"covs/claude-opus-4-6(xhigh)"},
		},
		[]byte(`{"error":{"message":"connection reset by peer"}}`),
		nil,
	)

	if summary.upstreamAttempts != 3 {
		t.Fatalf("upstreamAttempts = %d, want 3", summary.upstreamAttempts)
	}
	if summary.upstreamStatusText != "500 -> 401 -> transport_error" {
		t.Fatalf("upstreamStatusText = %q", summary.upstreamStatusText)
	}
	if !strings.Contains(summary.transportError, "connection reset by peer") {
		t.Fatalf("transportError = %q", summary.transportError)
	}
	if summary.replyTailComplete {
		t.Fatalf("replyTailComplete = true, want false")
	}
	if summary.toolCallSeen {
		t.Fatalf("toolCallSeen = true, want false")
	}
}

func TestFileStreamingLogWriter_WritesSummary(t *testing.T) {
	t.Parallel()

	logger := NewFileRequestLogger(true, t.TempDir(), "", 0)
	logger.SetStreamBodyLimit(1024)

	writerAny, err := logger.LogStreamingRequest(
		"/v1/chat/completions",
		"POST",
		map[string][]string{
			"User-Agent":   {"claude-cli/2.1.72 (external, cli)"},
			"Content-Type": {"application/json"},
		},
		[]byte(`{"stream":true}`),
		"stream-summary-test",
	)
	if err != nil {
		t.Fatalf("LogStreamingRequest() error = %v", err)
	}

	writer, ok := writerAny.(*FileStreamingLogWriter)
	if !ok {
		t.Fatalf("LogStreamingRequest() type = %T, want *FileStreamingLogWriter", writerAny)
	}

	if err := writer.WriteStatus(200, map[string][]string{
		"Content-Type":            {"text/event-stream"},
		"X-Cpams-Channel":         {"claude"},
		"X-Cpams-Decision":        {"reuse_sticky_selection"},
		"X-Cpams-Requested-Model": {"claude-opus-4-6(xhigh)"},
		"X-Cpams-Model":           {"covs/claude-opus-4-6(xhigh)"},
	}); err != nil {
		t.Fatalf("WriteStatus() error = %v", err)
	}

	if err := writer.WriteAPIResponse([]byte("=== API RESPONSE 1 ===\nStatus: 200\n")); err != nil {
		t.Fatalf("WriteAPIResponse() error = %v", err)
	}

	writer.WriteChunkAsync([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"TodoWrite\"}}]}}]}\n\n"))
	writer.WriteChunkAsync([]byte("event: message_stop\n\ndata: [DONE]\n"))

	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(writer.logFilePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	content := string(data)
	for _, needle := range []string{
		"=== SUMMARY ===",
		"tool_call_seen: yes",
		"first_tool_name: TodoWrite",
		"message_stop_seen: yes",
		"done_seen: yes",
		"reply_tail_complete: yes",
	} {
		if !strings.Contains(content, needle) {
			t.Fatalf("streaming log missing %q\nfull log:\n%s", needle, content)
		}
	}
}
