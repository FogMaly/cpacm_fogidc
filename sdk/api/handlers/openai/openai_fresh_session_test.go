package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestMaybeSanitizeFreshClaudeCLISession_StripsResumeTranscript(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Covs-Fresh-Session", "1")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("X-App", "cli")
	c.Request = req

	raw := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"system","content":"You are Claude Code."},
			{"role":"user","content":[
				{"type":"text","text":"<system-reminder>\nThe following skills are available for use with the Skill tool:\n</system-reminder>"},
				{"type":"text","text":"Resume this session with:"},
				{"type":"text","text":"继续处理这个问题"}
			]},
			{"role":"assistant","content":"old assistant output"},
			{"role":"tool","tool_call_id":"call_1","content":"old tool output"},
			{"role":"user","content":"现在给我一个结论"}
		]
	}`)

	sanitized := maybeSanitizeFreshClaudeCLISession(c, raw)
	messages := gjson.GetBytes(sanitized, "messages").Array()
	if len(messages) != 2 {
		t.Fatalf("messages length = %d, want 2; body=%s", len(messages), sanitized)
	}
	if got := messages[0].Get("role").String(); got != "system" {
		t.Fatalf("messages.0.role = %q, want system", got)
	}
	if got := messages[1].Get("role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user", got)
	}
	if got := messages[1].Get("content").String(); got != "现在给我一个结论" {
		t.Fatalf("messages.1.content = %q, want latest clean user prompt", got)
	}
}

func TestMaybeSanitizeFreshClaudeCLISession_PreservesCleanTailFromResumeMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Covs-Fresh-Session", "true")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("X-App", "cli")
	c.Request = req

	raw := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"system","content":"You are Claude Code."},
			{"role":"user","content":[
				{"type":"text","text":"Resume this session with:"},
				{"type":"text","text":"帮我重新总结当前状态"}
			]},
			{"role":"assistant","content":"old assistant output"}
		]
	}`)

	sanitized := maybeSanitizeFreshClaudeCLISession(c, raw)
	if got := gjson.GetBytes(sanitized, "messages.1.content.0.text").String(); got != "帮我重新总结当前状态" {
		t.Fatalf("messages.1.content.0.text = %q, want preserved clean tail", got)
	}
}

func TestMaybeSanitizeFreshClaudeCLISession_RequiresExplicitOptIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("X-App", "cli")
	c.Request = req

	raw := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"system","content":"sys"},{"role":"user","content":"Resume this session with:"}]}`)
	if got := string(maybeSanitizeFreshClaudeCLISession(c, raw)); got != string(raw) {
		t.Fatalf("body changed without opt-in: %s", got)
	}
}

func TestMaybeSanitizeFreshClaudeCLISession_IgnoresNonClaudeCLIClients(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Covs-Fresh-Session", "1")
	req.Header.Set("User-Agent", "curl/8.5.0")
	c.Request = req

	raw := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"system","content":"sys"},{"role":"user","content":"Resume this session with:"}]}`)
	if got := string(maybeSanitizeFreshClaudeCLISession(c, raw)); got != string(raw) {
		t.Fatalf("body changed for non-claude client: %s", got)
	}
}
