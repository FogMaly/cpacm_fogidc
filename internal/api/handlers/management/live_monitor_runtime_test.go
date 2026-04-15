package management

import (
	"testing"
	"time"
)

func TestLiveMonitorRuntime_RecoversAfterRouteFailureAndRestoresHistory(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Add(-1 * time.Minute)
	runtime := newLiveMonitorRuntime(t.TempDir())

	requestID := "req-live-monitor-1"
	clientIP := "203.0.113.24"
	requestedModel := "claude-sonnet-4-5"

	runtime.recordStart(liveMonitorPersistedEvent{
		Timestamp:      now,
		RequestID:      requestID,
		ClientIP:       clientIP,
		RequestedModel: requestedModel,
		RequestedType:  "claude",
	})
	runtime.recordRoute(liveMonitorPersistedEvent{
		Timestamp:      now.Add(1 * time.Second),
		RequestID:      requestID,
		RequestedModel: requestedModel,
		ActualModel:    "claude-sonnet-4-5-20250929",
		ActualType:     "claude",
		ChannelPrefix:  "k1dhp",
		AuthID:         "auth-k1",
		ResultStatus:   "selected",
	})
	runtime.recordRoute(liveMonitorPersistedEvent{
		Timestamp:      now.Add(2 * time.Second),
		RequestID:      requestID,
		RequestedModel: requestedModel,
		ActualModel:    "claude-sonnet-4-5-20250929",
		ActualType:     "claude",
		ChannelPrefix:  "k1dhp",
		AuthID:         "auth-k1",
		ResultStatus:   "failed",
		FallbackReason: "transport_error",
		ErrorMessage:   "write: connection reset by peer",
	})
	runtime.recordRoute(liveMonitorPersistedEvent{
		Timestamp:       now.Add(3 * time.Second),
		RequestID:       requestID,
		RequestedModel:  requestedModel,
		ActualModel:     "claude-sonnet-4-5-20250929",
		ActualType:      "claude",
		ChannelPrefix:   "covs",
		AuthID:          "auth-covs",
		ResultStatus:    "selected",
		StateTransition: "route_selected",
		FallbackReason:  "transport_error",
	})

	snapshot := runtime.snapshot(6*time.Hour, 1, 6, now.Add(4*time.Second))
	if snapshot.TotalCards != 1 {
		t.Fatalf("snapshot.TotalCards = %d, want 1", snapshot.TotalCards)
	}
	if len(snapshot.Cards) != 1 {
		t.Fatalf("len(snapshot.Cards) = %d, want 1", len(snapshot.Cards))
	}

	card := snapshot.Cards[0]
	if card.Status != liveMonitorStatusOnline {
		t.Fatalf("card.Status = %q, want %q", card.Status, liveMonitorStatusOnline)
	}
	if card.PrimaryChannelPrefix != "covs" {
		t.Fatalf("card.PrimaryChannelPrefix = %q, want covs", card.PrimaryChannelPrefix)
	}
	if card.MaskedIP != "203.0.113.*" {
		t.Fatalf("card.MaskedIP = %q, want 203.0.113.*", card.MaskedIP)
	}
	if card.DisplayModel != "claude-sonnet-4-5-20250929" {
		t.Fatalf("card.DisplayModel = %q, want actual model", card.DisplayModel)
	}
	if len(card.RecentIssues) == 0 {
		t.Fatalf("card.RecentIssues should not be empty after fallback")
	}

	detail, ok := runtime.detail(card.CardID, 6*time.Hour, now.Add(4*time.Second))
	if !ok {
		t.Fatalf("detail() returned ok=false")
	}
	if detail.FullIP != clientIP {
		t.Fatalf("detail.FullIP = %q, want %q", detail.FullIP, clientIP)
	}
	if detail.Status != liveMonitorStatusOnline {
		t.Fatalf("detail.Status = %q, want %q", detail.Status, liveMonitorStatusOnline)
	}
	if len(detail.ActiveRequests) != 1 {
		t.Fatalf("len(detail.ActiveRequests) = %d, want 1", len(detail.ActiveRequests))
	}
	if got := detail.ActiveRequests[0].ChannelPrefix; got != "covs" {
		t.Fatalf("detail.ActiveRequests[0].ChannelPrefix = %q, want covs", got)
	}
	if len(detail.IssueTimeline) == 0 {
		t.Fatalf("detail.IssueTimeline should not be empty")
	}

	runtime.recordCompletion(liveMonitorPersistedEvent{
		Timestamp:      now.Add(5 * time.Second),
		RequestID:      requestID,
		ClientIP:       clientIP,
		RequestedModel: requestedModel,
		RequestedType:  "claude",
		ResultStatus:   "success",
		HTTPStatus:     200,
	})

	offlineSnapshot := runtime.snapshot(6*time.Hour, 1, 6, now.Add(6*time.Second))
	if offlineSnapshot.TotalCards != 1 {
		t.Fatalf("offlineSnapshot.TotalCards = %d, want 1", offlineSnapshot.TotalCards)
	}
	offlineCard := offlineSnapshot.Cards[0]
	if offlineCard.Status != liveMonitorStatusOffline {
		t.Fatalf("offlineCard.Status = %q, want %q", offlineCard.Status, liveMonitorStatusOffline)
	}
	if offlineCard.RecentResultIcon != "switch" {
		t.Fatalf("offlineCard.RecentResultIcon = %q, want switch", offlineCard.RecentResultIcon)
	}

	restored := newLiveMonitorRuntime(runtime.store.dir)
	restoredSnapshot := restored.snapshot(24*time.Hour, 1, 6, now.Add(10*time.Second))
	if restoredSnapshot.TotalCards != 1 {
		t.Fatalf("restoredSnapshot.TotalCards = %d, want 1", restoredSnapshot.TotalCards)
	}
	restoredCard := restoredSnapshot.Cards[0]
	if restoredCard.Status != liveMonitorStatusOffline {
		t.Fatalf("restoredCard.Status = %q, want %q", restoredCard.Status, liveMonitorStatusOffline)
	}

	history, ok := restored.history(card.CardID, 24*time.Hour, 0, 20, now.Add(10*time.Second))
	if !ok {
		t.Fatalf("history() returned ok=false after restore")
	}
	if history.Total != 5 {
		t.Fatalf("history.Total = %d, want 5", history.Total)
	}
	if len(history.Items) != 5 {
		t.Fatalf("len(history.Items) = %d, want 5", len(history.Items))
	}
}
