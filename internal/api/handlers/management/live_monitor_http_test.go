package management

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	ilog "github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
)

func TestLiveMonitorMiddleware_TracksRequestsAndStreamPublishesCardEvents(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	handler := &Handler{
		liveMonitor: newLiveMonitorRuntime(t.TempDir()),
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		ilog.SetGinRequestID(c, "req-http-live-monitor")
		c.Next()
	})
	router.GET("/stream", handler.StreamLiveMonitor)
	router.GET("/cards", handler.GetLiveMonitorCards)
	router.POST("/v1/chat/completions", handler.LiveMonitorMiddleware(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	server := httptest.NewServer(router)
	defer server.Close()

	streamResp, err := http.Get(server.URL + "/stream")
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer streamResp.Body.Close()

	reader := bufio.NewReader(streamResp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read stream prelude: %v", err)
	}
	if !strings.HasPrefix(line, ": connected") {
		t.Fatalf("stream prelude = %q, want : connected", line)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("failed to read stream blank line: %v", err)
	}

	body := bytes.NewBufferString(`{"model":"gpt-5.3-codex"}`)
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("chat completion request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat completion status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	envelope := readLiveMonitorStreamEnvelope(t, reader)
	if envelope.Type != "card_touch" {
		t.Fatalf("envelope.Type = %q, want card_touch", envelope.Type)
	}
	if envelope.TotalCards != 1 {
		t.Fatalf("envelope.TotalCards = %d, want 1", envelope.TotalCards)
	}
	if envelope.Card == nil {
		t.Fatalf("envelope.Card is nil")
	}
	if envelope.Card.RequestedModel != "gpt-5.3-codex" {
		t.Fatalf("envelope.Card.RequestedModel = %q, want gpt-5.3-codex", envelope.Card.RequestedModel)
	}
	if envelope.Card.MaskedIP != "127.0.0.*" {
		t.Fatalf("envelope.Card.MaskedIP = %q, want 127.0.0.*", envelope.Card.MaskedIP)
	}

	cardsResp, err := http.Get(server.URL + "/cards?range=6h&page=1&page_size=6")
	if err != nil {
		t.Fatalf("cards request failed: %v", err)
	}
	defer cardsResp.Body.Close()
	if cardsResp.StatusCode != http.StatusOK {
		t.Fatalf("cards status = %d, want 200", cardsResp.StatusCode)
	}

	var page liveMonitorCardPage
	if err := json.NewDecoder(cardsResp.Body).Decode(&page); err != nil {
		t.Fatalf("decode cards response: %v", err)
	}
	if page.TotalCards != 1 {
		t.Fatalf("page.TotalCards = %d, want 1", page.TotalCards)
	}
	if len(page.Cards) != 1 {
		t.Fatalf("len(page.Cards) = %d, want 1", len(page.Cards))
	}
	if got := page.Cards[0].RequestedModel; got != "gpt-5.3-codex" {
		t.Fatalf("page.Cards[0].RequestedModel = %q, want gpt-5.3-codex", got)
	}
}

func TestLiveMonitorMiddleware_PrefersForwardedPublicIP(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	handler := &Handler{
		liveMonitor: newLiveMonitorRuntime(t.TempDir()),
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		ilog.SetGinRequestID(c, "req-http-live-monitor-forwarded")
		c.Next()
	})
	router.GET("/cards", handler.GetLiveMonitorCards)
	router.GET("/cards/:card_id", handler.GetLiveMonitorCard)
	router.POST("/v1/chat/completions", handler.LiveMonitorMiddleware(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	server := httptest.NewServer(router)
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Real-IP", "10.0.0.8")
	req.Header.Set("X-Forwarded-For", "10.0.0.8, 198.51.100.77")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	cardsResp, err := http.Get(server.URL + "/cards?range=6h&page=1&page_size=6")
	if err != nil {
		t.Fatalf("cards request failed: %v", err)
	}
	defer cardsResp.Body.Close()

	var page liveMonitorCardPage
	if err := json.NewDecoder(cardsResp.Body).Decode(&page); err != nil {
		t.Fatalf("decode cards response: %v", err)
	}
	if len(page.Cards) != 1 {
		t.Fatalf("len(page.Cards) = %d, want 1", len(page.Cards))
	}
	if got := page.Cards[0].MaskedIP; got != "198.51.100.*" {
		t.Fatalf("page.Cards[0].MaskedIP = %q, want %q", got, "198.51.100.*")
	}

	detailResp, err := http.Get(server.URL + "/cards/" + page.Cards[0].CardID + "?range=6h")
	if err != nil {
		t.Fatalf("detail request failed: %v", err)
	}
	defer detailResp.Body.Close()

	var detail liveMonitorDetail
	if err := json.NewDecoder(detailResp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail response: %v", err)
	}
	if got := detail.FullIP; got != "198.51.100.77" {
		t.Fatalf("detail.FullIP = %q, want %q", got, "198.51.100.77")
	}
}

func readLiveMonitorStreamEnvelope(t *testing.T, reader *bufio.Reader) liveMonitorStreamEnvelope {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read stream line: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		var envelope liveMonitorStreamEnvelope
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			t.Fatalf("decode stream payload: %v", err)
		}
		return envelope
	}
	t.Fatalf("timed out waiting for live monitor SSE payload")
	return liveMonitorStreamEnvelope{}
}
