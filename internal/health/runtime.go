package health

import "time"

type RuntimeLoadSnapshotItem struct {
	AuthID         string
	Provider       string
	Prefix         string
	BaseURL        string
	Label          string
	Inflight       int
	RecentRPS      float64
	ErrorRate      float64
	P95LatencyMS   float64
	LastSelectedAt time.Time
	CooldownUntil  time.Time
}

type ProviderRuntimeLoadSnapshotItem struct {
	Provider       string
	Inflight       int
	RecentRPS      float64
	ErrorRate      float64
	P95LatencyMS   float64
	LastSelectedAt time.Time
	CooldownUntil  time.Time
}
