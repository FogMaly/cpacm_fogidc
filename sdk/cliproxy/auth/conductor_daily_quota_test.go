package auth

import (
	"context"
	"testing"
	"time"
)

func TestIsDailyQuotaExceededError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *Error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "daily limit marker",
			err:  &Error{Message: `{"error":{"type":"daily_limit_reached","message":"Daily spending limit reached. Your quota will reset tomorrow."}}`},
			want: true,
		},
		{
			name: "daily spending phrase",
			err:  &Error{Message: "Daily spending limit reached. You have spent all quota."},
			want: true,
		},
		{
			name: "generic payment required",
			err:  &Error{Message: "payment required"},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isDailyQuotaExceededError(tc.err)
			if got != tc.want {
				t.Fatalf("isDailyQuotaExceededError() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestNextDailyQuotaResumeAtChinaMidnightUTC(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 3, 8, 30, 0, 0, time.UTC)
	got := nextDailyQuotaResumeAtChinaMidnightUTC(now)
	want := time.Date(2026, 3, 3, 16, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("nextDailyQuotaResumeAtChinaMidnightUTC(%s) = %s, want %s", now.Format(time.RFC3339), got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestApplyAuthFailureState_DailyQuota402(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 3, 8, 30, 0, 0, time.UTC)
	auth := &Auth{}
	resultErr := &Error{
		HTTPStatus: 402,
		Message:    "Daily spending limit reached. You have spent all quota. Your quota will reset tomorrow.",
	}
	applyAuthFailureState(auth, resultErr, nil, now)

	wantRetry := time.Date(2026, 3, 3, 16, 0, 0, 0, time.UTC)
	if !auth.NextRetryAfter.Equal(wantRetry) {
		t.Fatalf("auth.NextRetryAfter = %s, want %s", auth.NextRetryAfter.Format(time.RFC3339), wantRetry.Format(time.RFC3339))
	}
	if !auth.Quota.Exceeded {
		t.Fatalf("auth.Quota.Exceeded = false, want true")
	}
	if auth.Quota.Reason != "daily_quota" {
		t.Fatalf("auth.Quota.Reason = %q, want %q", auth.Quota.Reason, "daily_quota")
	}
	if !auth.Quota.NextRecoverAt.Equal(wantRetry) {
		t.Fatalf("auth.Quota.NextRecoverAt = %s, want %s", auth.Quota.NextRecoverAt.Format(time.RFC3339), wantRetry.Format(time.RFC3339))
	}
}

func TestApplyAuthFailureState_Generic402(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 3, 8, 30, 0, 0, time.UTC)
	auth := &Auth{}
	resultErr := &Error{
		HTTPStatus: 402,
		Message:    "payment required",
	}
	applyAuthFailureState(auth, resultErr, nil, now)

	wantRetry := now.Add(30 * time.Minute)
	if auth.NextRetryAfter.Sub(wantRetry) > time.Second || wantRetry.Sub(auth.NextRetryAfter) > time.Second {
		t.Fatalf("auth.NextRetryAfter = %s, want around %s", auth.NextRetryAfter.Format(time.RFC3339), wantRetry.Format(time.RFC3339))
	}
	if auth.Quota.Reason == "daily_quota" {
		t.Fatalf("auth.Quota.Reason = daily_quota, want non-daily for generic 402")
	}
}

func TestManagerMarkResult_ModelDailyQuota402(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	if _, err := mgr.Register(context.Background(), &Auth{
		ID:       "auth-daily",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	before := time.Now().UTC()
	mgr.MarkResult(context.Background(), Result{
		AuthID:   "auth-daily",
		Provider: "codex",
		Model:    "gpt-5.2",
		Success:  false,
		Error: &Error{
			HTTPStatus: 402,
			Message:    "Daily spending limit reached. Your quota will reset tomorrow.",
		},
	})
	after := time.Now().UTC()

	updated, ok := mgr.GetByID("auth-daily")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates["gpt-5.2"]
	if state == nil {
		t.Fatalf("expected model state for gpt-5.2")
	}
	if !state.Quota.Exceeded {
		t.Fatalf("state.Quota.Exceeded = false, want true")
	}
	if state.Quota.Reason != "daily_quota" {
		t.Fatalf("state.Quota.Reason = %q, want %q", state.Quota.Reason, "daily_quota")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatalf("state.NextRetryAfter is zero, want daily quota resume time")
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("state.Quota.NextRecoverAt = %s, want %s", state.Quota.NextRecoverAt.Format(time.RFC3339), state.NextRetryAfter.Format(time.RFC3339))
	}

	wantLower := nextDailyQuotaResumeAtChinaMidnightUTC(before)
	wantUpper := nextDailyQuotaResumeAtChinaMidnightUTC(after)
	minWant := wantLower
	maxWant := wantUpper
	if maxWant.Before(minWant) {
		minWant, maxWant = maxWant, minWant
	}
	if state.NextRetryAfter.Before(minWant) || state.NextRetryAfter.After(maxWant) {
		t.Fatalf(
			"state.NextRetryAfter = %s, want between %s and %s",
			state.NextRetryAfter.Format(time.RFC3339),
			minWant.Format(time.RFC3339),
			maxWant.Format(time.RFC3339),
		)
	}
}
