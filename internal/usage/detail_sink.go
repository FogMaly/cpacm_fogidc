package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultDetailQueueSize    = 4096
	detailWriterFlushInterval = 2 * time.Second
	detailCleanupInterval     = 6 * time.Hour
)

type persistedDetailRecord struct {
	Timestamp time.Time  `json:"timestamp"`
	APIKey    string     `json:"api_key"`
	Provider  string     `json:"provider,omitempty"`
	Model     string     `json:"model"`
	Source    string     `json:"source,omitempty"`
	AuthID    string     `json:"auth_id,omitempty"`
	AuthIndex string     `json:"auth_index,omitempty"`
	Failed    bool       `json:"failed"`
	Tokens    TokenStats `json:"tokens"`
}

type DetailPersistenceStatus struct {
	Enabled     bool      `json:"enabled"`
	Directory   string    `json:"directory,omitempty"`
	QueueDepth  int       `json:"queue_depth"`
	QueueCap    int       `json:"queue_capacity"`
	Dropped     uint64    `json:"dropped"`
	WriteErrors uint64    `json:"write_errors"`
	LastWriteAt time.Time `json:"last_write_at,omitempty"`
}

type detailSink struct {
	dir       string
	retention time.Duration
	queue     chan persistedDetailRecord
	stop      chan struct{}
	done      chan struct{}

	fileMu      sync.Mutex
	currentDate string
	currentFile *os.File
	currentBuf  *bufio.Writer

	dropped     atomic.Uint64
	writeErrors atomic.Uint64
	lastWriteAt atomic.Int64
}

func newDetailSink(dir string, retention time.Duration) (*detailSink, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("detail sink directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	sink := &detailSink{
		dir:       dir,
		retention: normalizeDetailsRetention(retention),
		queue:     make(chan persistedDetailRecord, defaultDetailQueueSize),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	sink.cleanupExpiredFiles(time.Now().UTC())
	go sink.run()
	return sink, nil
}

func (s *detailSink) Enqueue(record persistedDetailRecord) {
	if s == nil {
		return
	}
	select {
	case s.queue <- record:
	default:
		s.dropped.Add(1)
	}
}

func (s *detailSink) Close() {
	if s == nil {
		return
	}
	select {
	case <-s.done:
		return
	default:
	}
	close(s.stop)
	<-s.done
}

func (s *detailSink) Status() DetailPersistenceStatus {
	if s == nil {
		return DetailPersistenceStatus{}
	}
	status := DetailPersistenceStatus{
		Enabled:     true,
		Directory:   s.dir,
		QueueDepth:  len(s.queue),
		QueueCap:    cap(s.queue),
		Dropped:     s.dropped.Load(),
		WriteErrors: s.writeErrors.Load(),
	}
	if last := s.lastWriteAt.Load(); last > 0 {
		status.LastWriteAt = time.Unix(0, last).UTC()
	}
	return status
}

func (s *detailSink) run() {
	defer close(s.done)

	flushTicker := time.NewTicker(detailWriterFlushInterval)
	defer flushTicker.Stop()
	cleanupTicker := time.NewTicker(detailCleanupInterval)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-s.stop:
			s.drainAndClose()
			return
		case <-flushTicker.C:
			s.flushCurrent()
		case now := <-cleanupTicker.C:
			s.cleanupExpiredFiles(now.UTC())
		case record := <-s.queue:
			s.writeRecord(record)
		}
	}
}

func (s *detailSink) drainAndClose() {
	if s == nil {
		return
	}
	for {
		select {
		case record := <-s.queue:
			s.writeRecord(record)
		default:
			s.flushAndClose()
			return
		}
	}
}

func (s *detailSink) writeRecord(record persistedDetailRecord) {
	record.Timestamp = record.Timestamp.UTC()
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}
	line, err := json.Marshal(record)
	if err != nil {
		s.writeErrors.Add(1)
		return
	}

	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	if err := s.ensureWriterLocked(record.Timestamp); err != nil {
		s.writeErrors.Add(1)
		return
	}
	if s.currentBuf == nil {
		s.writeErrors.Add(1)
		return
	}
	if _, err := s.currentBuf.Write(line); err != nil {
		s.writeErrors.Add(1)
		return
	}
	if err := s.currentBuf.WriteByte('\n'); err != nil {
		s.writeErrors.Add(1)
		return
	}
	s.lastWriteAt.Store(time.Now().UTC().UnixNano())
}

func (s *detailSink) ensureWriterLocked(ts time.Time) error {
	dateKey := ts.Format("2006-01-02")
	if s.currentBuf != nil && s.currentDate == dateKey {
		return nil
	}
	if err := s.flushAndCloseLocked(); err != nil {
		return err
	}
	path := filepath.Join(s.dir, dateKey+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	s.currentDate = dateKey
	s.currentFile = file
	s.currentBuf = bufio.NewWriterSize(file, 64*1024)
	return nil
}

func (s *detailSink) flushCurrent() {
	if s == nil {
		return
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if s.currentBuf != nil {
		_ = s.currentBuf.Flush()
	}
}

func (s *detailSink) flushAndClose() {
	if s == nil {
		return
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	_ = s.flushAndCloseLocked()
}

func (s *detailSink) flushAndCloseLocked() error {
	var firstErr error
	if s.currentBuf != nil {
		if err := s.currentBuf.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.currentFile != nil {
		if err := s.currentFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.currentDate = ""
	s.currentFile = nil
	s.currentBuf = nil
	return firstErr
}

func (s *detailSink) cleanupExpiredFiles(now time.Time) {
	if s == nil || s.retention <= 0 {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-s.retention)
	cutoffDay := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		dayText := strings.TrimSuffix(name, ".jsonl")
		day, err := time.Parse("2006-01-02", dayText)
		if err != nil {
			continue
		}
		if day.Before(cutoffDay) {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
}
