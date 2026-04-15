package auth

import (
	"context"
	"strings"
	"sync"
)

type resultSignalContextKey struct{}

type RuntimeOutageSignal struct {
	Status int
	Reason string
}

type ResultSignalCollector struct {
	mu            sync.Mutex
	runtimeOutage RuntimeOutageSignal
}

func WithResultSignalCollector(ctx context.Context) (context.Context, *ResultSignalCollector) {
	if ctx == nil {
		ctx = context.Background()
	}
	collector := &ResultSignalCollector{}
	return context.WithValue(ctx, resultSignalContextKey{}, collector), collector
}

func ReportRuntimeOutageSignal(ctx context.Context, status int, reason string) {
	reason = strings.TrimSpace(reason)
	if ctx == nil || reason == "" {
		return
	}
	collector, _ := ctx.Value(resultSignalContextKey{}).(*ResultSignalCollector)
	if collector == nil {
		return
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if collector.runtimeOutage.Reason == "" || status >= collector.runtimeOutage.Status {
		collector.runtimeOutage = RuntimeOutageSignal{
			Status: status,
			Reason: reason,
		}
	}
}

func (c *ResultSignalCollector) RuntimeOutageSignal() (RuntimeOutageSignal, bool) {
	if c == nil {
		return RuntimeOutageSignal{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(c.runtimeOutage.Reason) == "" {
		return RuntimeOutageSignal{}, false
	}
	return c.runtimeOutage, true
}

func applyResultSignals(result *Result, collector *ResultSignalCollector) {
	if result == nil || collector == nil {
		return
	}
	outage, ok := collector.RuntimeOutageSignal()
	if !ok {
		return
	}
	result.RuntimeOutageStatus = outage.Status
	result.RuntimeOutageReason = outage.Reason
}
