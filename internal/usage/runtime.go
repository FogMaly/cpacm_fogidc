package usage

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	defaultDetailsMemoryWindow = time.Hour
	defaultDetailsRetention    = 7 * 24 * time.Hour
)

type RuntimeConfig struct {
	Enabled             bool
	DetailsStateDir     string
	DetailsMemoryWindow time.Duration
	DetailsRetention    time.Duration
}

type RuntimeStatus struct {
	Enabled                 bool                    `json:"enabled"`
	StoreDetailsInMemory    bool                    `json:"store_details_in_memory"`
	DetailsMemoryWindowMins int64                   `json:"details_memory_window_minutes"`
	DetailsRetentionDays    int64                   `json:"details_retention_days"`
	Details                 DetailPersistenceStatus `json:"details"`
	DispatchQueue           coreusage.QueueStatus   `json:"dispatch_queue"`
}

var detailsMemoryWindowNanos atomic.Int64

func init() {
	detailsMemoryWindowNanos.Store(int64(defaultDetailsMemoryWindow))
}

type runtimeState struct {
	mu   sync.RWMutex
	sink *detailSink
	cfg  RuntimeConfig
}

var defaultRuntime runtimeState

func ResolveStateDir(configFilePath, cwd string) string {
	base := util.WritablePath()
	if strings.TrimSpace(base) == "" {
		configPath := strings.TrimSpace(configFilePath)
		switch {
		case configPath != "":
			base = filepath.Dir(configPath)
		case strings.TrimSpace(cwd) != "":
			base = cwd
		default:
			base = "."
		}
	}
	return filepath.Join(base, ".cpapi-state")
}

func ConfigureRuntime(cfg RuntimeConfig) {
	cfg.DetailsMemoryWindow = normalizeDetailsMemoryWindow(cfg.DetailsMemoryWindow)
	cfg.DetailsRetention = normalizeDetailsRetention(cfg.DetailsRetention)
	SetDetailsMemoryWindow(cfg.DetailsMemoryWindow)

	cleanDir := strings.TrimSpace(cfg.DetailsStateDir)
	if cleanDir != "" {
		cfg.DetailsStateDir = cleanDir
	}

	defaultRuntime.mu.Lock()
	defer defaultRuntime.mu.Unlock()

	if defaultRuntime.sink != nil {
		defaultRuntime.sink.Close()
		defaultRuntime.sink = nil
	}
	defaultRuntime.cfg = cfg

	if !cfg.Enabled || cfg.DetailsStateDir == "" {
		return
	}

	sink, err := newDetailSink(cfg.DetailsStateDir, cfg.DetailsRetention)
	if err != nil {
		log.WithError(err).Warn("usage: failed to start detail sink")
		return
	}
	defaultRuntime.sink = sink
}

func RuntimeSnapshot() RuntimeStatus {
	defaultRuntime.mu.RLock()
	sink := defaultRuntime.sink
	cfg := defaultRuntime.cfg
	defaultRuntime.mu.RUnlock()

	status := RuntimeStatus{
		StoreDetailsInMemory:    sink == nil,
		Enabled:                 cfg.Enabled,
		DetailsMemoryWindowMins: int64(normalizeDetailsMemoryWindow(cfg.DetailsMemoryWindow) / time.Minute),
		DetailsRetentionDays:    int64(normalizeDetailsRetention(cfg.DetailsRetention) / (24 * time.Hour)),
		DispatchQueue:           coreusage.DefaultQueueStatus(),
	}
	if sink != nil {
		status.Details = sink.Status()
	}
	return status
}

func SetDetailsMemoryWindow(window time.Duration) {
	detailsMemoryWindowNanos.Store(int64(normalizeDetailsMemoryWindow(window)))
}

func DetailsMemoryWindow() time.Duration {
	window := time.Duration(detailsMemoryWindowNanos.Load())
	return normalizeDetailsMemoryWindow(window)
}

func DetailsRetention() time.Duration {
	defaultRuntime.mu.RLock()
	retention := defaultRuntime.cfg.DetailsRetention
	defaultRuntime.mu.RUnlock()
	return normalizeDetailsRetention(retention)
}

func StoreDetailsInMemory() bool {
	defaultRuntime.mu.RLock()
	sink := defaultRuntime.sink
	defaultRuntime.mu.RUnlock()
	return sink == nil
}

func enqueueDetailRecord(record persistedDetailRecord) {
	defaultRuntime.mu.RLock()
	sink := defaultRuntime.sink
	defaultRuntime.mu.RUnlock()
	if sink != nil {
		sink.Enqueue(record)
	}
}

func normalizeDetailsMemoryWindow(window time.Duration) time.Duration {
	if window <= 0 {
		return defaultDetailsMemoryWindow
	}
	return window
}

func normalizeDetailsRetention(retention time.Duration) time.Duration {
	if retention <= 0 {
		return defaultDetailsRetention
	}
	return retention
}
