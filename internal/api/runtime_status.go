package api

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type processMemoryStatus struct {
	AllocBytes       uint64  `json:"alloc_bytes"`
	HeapAllocBytes   uint64  `json:"heap_alloc_bytes"`
	HeapSysBytes     uint64  `json:"heap_sys_bytes"`
	HeapIdleBytes    uint64  `json:"heap_idle_bytes"`
	HeapInuseBytes   uint64  `json:"heap_inuse_bytes"`
	StackInuseBytes  uint64  `json:"stack_inuse_bytes"`
	SysBytes         uint64  `json:"sys_bytes"`
	NextGCBytes      uint64  `json:"next_gc_bytes"`
	GCCPUFraction    float64 `json:"gc_cpu_fraction"`
	NumGC            uint32  `json:"num_gc"`
	ResidentSetBytes uint64  `json:"resident_set_bytes,omitempty"`
}

type processRuntimeStatus struct {
	PID          int                 `json:"pid"`
	Goroutines   int                 `json:"goroutines"`
	GOMAXPROCS   int                 `json:"gomaxprocs"`
	NumCPU       int                 `json:"num_cpu"`
	Memory       processMemoryStatus `json:"memory"`
	GeneratedAt  time.Time           `json:"generated_at"`
	ServerConfig struct {
		LoggingToFile     bool `json:"logging_to_file"`
		RequestLogEnabled bool `json:"request_log_enabled"`
	} `json:"server_config"`
}

type logDirectoryStatus struct {
	Path           string `json:"path,omitempty"`
	FileCount      int    `json:"file_count"`
	TotalSizeBytes int64  `json:"total_size_bytes"`
	MaxTotalSizeMB int    `json:"max_total_size_mb"`
}

func (s *Server) getRuntimeStatus(c *gin.Context) {
	if s == nil || s.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server_config_unavailable"})
		return
	}

	status := gin.H{
		"process":            buildProcessRuntimeStatus(s.cfg),
		"usage":              usage.RuntimeSnapshot(),
		"inbound_rate_limit": s.runtimeLimiterStatus(),
		"logs":               buildLogDirectoryStatus(s.cfg),
	}

	c.JSON(http.StatusOK, status)
}

func (s *Server) runtimeLimiterStatus() inboundLimiterStatus {
	if s == nil || s.inboundLimiter == nil {
		return inboundLimiterStatus{}
	}
	return s.inboundLimiter.Status()
}

func buildProcessRuntimeStatus(cfg *config.Config) processRuntimeStatus {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	status := processRuntimeStatus{
		PID:         os.Getpid(),
		Goroutines:  runtime.NumGoroutine(),
		GOMAXPROCS:  runtime.GOMAXPROCS(0),
		NumCPU:      runtime.NumCPU(),
		GeneratedAt: time.Now().UTC(),
		Memory: processMemoryStatus{
			AllocBytes:      mem.Alloc,
			HeapAllocBytes:  mem.HeapAlloc,
			HeapSysBytes:    mem.HeapSys,
			HeapIdleBytes:   mem.HeapIdle,
			HeapInuseBytes:  mem.HeapInuse,
			StackInuseBytes: mem.StackInuse,
			SysBytes:        mem.Sys,
			NextGCBytes:     mem.NextGC,
			GCCPUFraction:   mem.GCCPUFraction,
			NumGC:           mem.NumGC,
		},
	}
	if cfg != nil {
		status.ServerConfig.LoggingToFile = cfg.LoggingToFile
		status.ServerConfig.RequestLogEnabled = cfg.RequestLog
	}
	if rss, ok := readResidentSetBytes(); ok {
		status.Memory.ResidentSetBytes = rss
	}
	return status
}

func buildLogDirectoryStatus(cfg *config.Config) logDirectoryStatus {
	if cfg == nil {
		return logDirectoryStatus{}
	}

	logDir := strings.TrimSpace(logging.ResolveLogDirectory(cfg))
	status := logDirectoryStatus{
		Path:           logDir,
		MaxTotalSizeMB: cfg.LogsMaxTotalSizeMB,
	}
	if logDir == "" {
		return status
	}

	_ = filepath.Walk(logDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		status.FileCount++
		status.TotalSizeBytes += info.Size()
		return nil
	})
	return status
}

func readResidentSetBytes() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	const prefix = "VmRSS:"
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		valueKB, errConv := strconv.ParseUint(fields[1], 10, 64)
		if errConv != nil {
			return 0, false
		}
		return valueKB * 1024, true
	}
	return 0, false
}
