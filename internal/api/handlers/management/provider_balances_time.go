package management

import (
	"fmt"
	"time"
)

func resetCountdownAtChinaTime(now time.Time, hour, minute int) string {
	return formatCountdown(nextResetAtChinaTime(now, hour, minute).Sub(now))
}

func nextResetAtChinaTime(now time.Time, hour, minute int) time.Time {
	localNow := now.In(time.FixedZone("UTC+8", 8*3600))
	if hour >= 24 {
		startOfNextDay := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, localNow.Location()).Add(24 * time.Hour)
		return startOfNextDay.Add(time.Duration(minute) * time.Minute)
	}
	nextReset := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, localNow.Location())
	if !localNow.Before(nextReset) {
		nextReset = nextReset.Add(24 * time.Hour)
	}
	return nextReset
}

func appendResetCountdown(detail string, now, resetAt time.Time) string {
	if detail == "" || resetAt.IsZero() {
		return detail
	}
	return detail + fmt.Sprintf(" | %s 后重置", formatCountdown(resetAt.Sub(now)))
}

func formatCountdown(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	totalMinutes := int((d + time.Minute - time.Nanosecond) / time.Minute)
	if totalMinutes <= 0 {
		totalMinutes = 1
	}
	hours := totalMinutes / 60
	minutes := totalMinutes % 60
	if hours <= 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh %dm", hours, minutes)
}
