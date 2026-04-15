package health

import (
	"testing"
	"time"
)

func TestScore_YellowProbeIsDegraded(t *testing.T) {
	now := time.Date(2026, 4, 1, 15, 0, 0, 0, time.UTC)

	score := Score(UnifiedSignals{
		Probe: ProbeSignal{
			Status:    "yellow",
			TestedAt:  now,
			UpdatedAt: now,
			Found:     true,
		},
	}, now)

	if score.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", score.Status)
	}
	if !score.Available {
		t.Fatal("available = false, want true")
	}
}

func TestScore_RedProbeWithoutRuntimeIsUnavailable(t *testing.T) {
	now := time.Date(2026, 4, 1, 15, 0, 0, 0, time.UTC)

	score := Score(UnifiedSignals{
		Probe: ProbeSignal{
			Status:    "red",
			TestedAt:  now,
			UpdatedAt: now,
			Found:     true,
		},
	}, now)

	if score.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", score.Status)
	}
	if score.Available {
		t.Fatal("available = true, want false")
	}
}
