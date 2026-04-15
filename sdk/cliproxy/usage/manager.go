package usage

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Record contains the usage statistics captured for a single provider request.
type Record struct {
	Provider    string
	Model       string
	APIKey      string
	AuthID      string
	AuthIndex   string
	Source      string
	RequestedAt time.Time
	Failed      bool
	Detail      Detail
}

// Detail holds the token usage breakdown.
type Detail struct {
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	TotalTokens     int64
}

// Plugin consumes usage records emitted by the proxy runtime.
type Plugin interface {
	HandleUsage(ctx context.Context, record Record)
}

type queueItem struct {
	ctx    context.Context
	record Record
}

// Manager maintains a queue of usage records and delivers them to registered plugins.
type Manager struct {
	once     sync.Once
	stopOnce sync.Once
	cancel   context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []queueItem
	closed  bool
	buffer  int
	dropped atomic.Uint64

	pluginsMu sync.RWMutex
	plugins   []Plugin
}

// NewManager constructs a manager with a buffered queue.
func NewManager(buffer int) *Manager {
	if buffer <= 0 {
		buffer = 512
	}
	m := &Manager{buffer: buffer}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// SetBuffer updates the maximum number of queued usage records retained in memory.
func (m *Manager) SetBuffer(buffer int) {
	if m == nil || buffer <= 0 {
		return
	}
	m.mu.Lock()
	m.buffer = buffer
	for len(m.queue) > m.buffer {
		m.queue = m.queue[1:]
		m.dropped.Add(1)
	}
	m.mu.Unlock()
}

// Start launches the background dispatcher. Calling Start multiple times is safe.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.once.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		var workerCtx context.Context
		workerCtx, m.cancel = context.WithCancel(ctx)
		go m.run(workerCtx)
	})
}

// Stop stops the dispatcher and drains the queue.
func (m *Manager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.cond.Broadcast()
	})
}

// Register appends a plugin to the delivery list.
func (m *Manager) Register(plugin Plugin) {
	if m == nil || plugin == nil {
		return
	}
	m.pluginsMu.Lock()
	m.plugins = append(m.plugins, plugin)
	m.pluginsMu.Unlock()
}

// Publish enqueues a usage record for processing. If no plugin is registered
// the record will be discarded downstream.
func (m *Manager) Publish(ctx context.Context, record Record) {
	if m == nil {
		return
	}
	// ensure worker is running even if Start was not called explicitly
	m.Start(context.Background())
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if m.buffer > 0 && len(m.queue) >= m.buffer {
		m.dropped.Add(1)
		m.mu.Unlock()
		return
	}
	m.queue = append(m.queue, queueItem{ctx: ctx, record: record})
	m.mu.Unlock()
	m.cond.Signal()
}

// QueueStatus summarises the manager's in-memory queue state.
func (m *Manager) QueueStatus() QueueStatus {
	if m == nil {
		return QueueStatus{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return QueueStatus{
		Depth:    len(m.queue),
		Capacity: m.buffer,
		Dropped:  m.dropped.Load(),
	}
}

func (m *Manager) run(ctx context.Context) {
	for {
		m.mu.Lock()
		for !m.closed && len(m.queue) == 0 {
			m.cond.Wait()
		}
		if len(m.queue) == 0 && m.closed {
			m.mu.Unlock()
			return
		}
		item := m.queue[0]
		m.queue = m.queue[1:]
		m.mu.Unlock()
		m.dispatch(item)
	}
}

func (m *Manager) dispatch(item queueItem) {
	m.pluginsMu.RLock()
	plugins := make([]Plugin, len(m.plugins))
	copy(plugins, m.plugins)
	m.pluginsMu.RUnlock()
	if len(plugins) == 0 {
		return
	}
	for _, plugin := range plugins {
		if plugin == nil {
			continue
		}
		safeInvoke(plugin, item.ctx, item.record)
	}
}

func safeInvoke(plugin Plugin, ctx context.Context, record Record) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("usage: plugin panic recovered: %v", r)
		}
	}()
	plugin.HandleUsage(ctx, record)
}

var defaultManager = NewManager(512)

// QueueStatus describes the current usage manager queue state.
type QueueStatus struct {
	Depth    int    `json:"depth"`
	Capacity int    `json:"capacity"`
	Dropped  uint64 `json:"dropped"`
}

// DefaultManager returns the global usage manager instance.
func DefaultManager() *Manager { return defaultManager }

// RegisterPlugin registers a plugin on the default manager.
func RegisterPlugin(plugin Plugin) { DefaultManager().Register(plugin) }

// PublishRecord publishes a record using the default manager.
func PublishRecord(ctx context.Context, record Record) { DefaultManager().Publish(ctx, record) }

// StartDefault starts the default manager's dispatcher.
func StartDefault(ctx context.Context) { DefaultManager().Start(ctx) }

// StopDefault stops the default manager's dispatcher.
func StopDefault() { DefaultManager().Stop() }

// DefaultQueueStatus returns the queue status for the global usage manager.
func DefaultQueueStatus() QueueStatus { return DefaultManager().QueueStatus() }
