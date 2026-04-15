package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func (s *Server) getMetrics(c *gin.Context) {
	if s == nil || s.cfg == nil {
		c.String(http.StatusServiceUnavailable, "server_config_unavailable\n")
		return
	}

	process := buildProcessRuntimeStatus(s.cfg)
	runtimeStatus := usage.RuntimeSnapshot()
	limiter := s.runtimeLimiterStatus()
	logs := buildLogDirectoryStatus(s.cfg)

	var out strings.Builder
	writeGaugeMetric(&out, "cpapi_process_goroutines", "Current number of goroutines.", float64(process.Goroutines))
	writeGaugeMetric(&out, "cpapi_process_gomaxprocs", "Current GOMAXPROCS setting.", float64(process.GOMAXPROCS))
	writeGaugeMetric(&out, "cpapi_process_num_cpu", "Number of CPUs visible to the process.", float64(process.NumCPU))
	writeGaugeMetric(&out, "cpapi_process_alloc_bytes", "Bytes of allocated heap objects.", float64(process.Memory.AllocBytes))
	writeGaugeMetric(&out, "cpapi_process_heap_alloc_bytes", "Bytes of allocated heap memory.", float64(process.Memory.HeapAllocBytes))
	writeGaugeMetric(&out, "cpapi_process_heap_sys_bytes", "Bytes of heap memory obtained from the OS.", float64(process.Memory.HeapSysBytes))
	writeGaugeMetric(&out, "cpapi_process_heap_idle_bytes", "Bytes in idle heap spans.", float64(process.Memory.HeapIdleBytes))
	writeGaugeMetric(&out, "cpapi_process_heap_inuse_bytes", "Bytes in in-use heap spans.", float64(process.Memory.HeapInuseBytes))
	writeGaugeMetric(&out, "cpapi_process_stack_inuse_bytes", "Bytes in stack spans.", float64(process.Memory.StackInuseBytes))
	writeGaugeMetric(&out, "cpapi_process_sys_bytes", "Total bytes of memory obtained from the OS.", float64(process.Memory.SysBytes))
	writeGaugeMetric(&out, "cpapi_process_next_gc_bytes", "Target heap size of the next GC cycle.", float64(process.Memory.NextGCBytes))
	writeGaugeMetric(&out, "cpapi_process_gc_cpu_fraction", "Fraction of CPU time used by GC.", process.Memory.GCCPUFraction)
	writeCounterMetric(&out, "cpapi_process_gc_cycles_total", "Total completed GC cycles.", float64(process.Memory.NumGC))
	writeGaugeMetric(&out, "cpapi_process_resident_set_bytes", "Resident set size reported by the OS.", float64(process.Memory.ResidentSetBytes))

	writeGaugeMetric(&out, "cpapi_config_logging_to_file", "Whether file logging is enabled (1=yes, 0=no).", boolToFloat(process.ServerConfig.LoggingToFile))
	writeGaugeMetric(&out, "cpapi_config_request_log_enabled", "Whether request logging is enabled (1=yes, 0=no).", boolToFloat(process.ServerConfig.RequestLogEnabled))

	writeGaugeMetric(&out, "cpapi_logs_file_count", "Number of files in the log directory.", float64(logs.FileCount))
	writeGaugeMetric(&out, "cpapi_logs_total_size_bytes", "Total size of files in the log directory.", float64(logs.TotalSizeBytes))
	writeGaugeMetric(&out, "cpapi_logs_max_total_size_megabytes", "Configured log directory size cap in megabytes.", float64(logs.MaxTotalSizeMB))

	writeGaugeMetric(&out, "cpapi_usage_runtime_enabled", "Whether runtime usage tracking is enabled (1=yes, 0=no).", boolToFloat(runtimeStatus.Enabled))
	writeGaugeMetric(&out, "cpapi_usage_store_details_in_memory", "Whether live usage details are retained in process memory (1=yes, 0=no).", boolToFloat(runtimeStatus.StoreDetailsInMemory))
	writeGaugeMetric(&out, "cpapi_usage_details_memory_window_minutes", "Configured in-memory usage detail window.", float64(runtimeStatus.DetailsMemoryWindowMins))
	writeGaugeMetric(&out, "cpapi_usage_details_retention_days", "Configured persisted usage detail retention.", float64(runtimeStatus.DetailsRetentionDays))
	writeGaugeMetric(&out, "cpapi_usage_dispatch_queue_depth", "Current depth of the usage dispatch queue.", float64(runtimeStatus.DispatchQueue.Depth))
	writeGaugeMetric(&out, "cpapi_usage_dispatch_queue_capacity", "Capacity of the usage dispatch queue.", float64(runtimeStatus.DispatchQueue.Capacity))
	writeCounterMetric(&out, "cpapi_usage_dispatch_queue_dropped_total", "Dropped usage records in the dispatch queue.", float64(runtimeStatus.DispatchQueue.Dropped))
	writeGaugeMetric(&out, "cpapi_usage_detail_queue_depth", "Current depth of the persisted usage detail queue.", float64(runtimeStatus.Details.QueueDepth))
	writeGaugeMetric(&out, "cpapi_usage_detail_queue_capacity", "Capacity of the persisted usage detail queue.", float64(runtimeStatus.Details.QueueCap))
	writeCounterMetric(&out, "cpapi_usage_detail_queue_dropped_total", "Dropped persisted usage detail records.", float64(runtimeStatus.Details.Dropped))
	writeCounterMetric(&out, "cpapi_usage_detail_write_errors_total", "Write errors when persisting usage details.", float64(runtimeStatus.Details.WriteErrors))

	writeGaugeMetric(&out, "cpapi_inbound_rate_limit_enabled", "Whether inbound rate limiting is enabled (1=yes, 0=no).", boolToFloat(limiter.Enabled))
	writeGaugeMetric(&out, "cpapi_inbound_rate_limit_per_key_qps", "Configured per-key QPS limit.", limiter.PerKeyQPS)
	writeGaugeMetric(&out, "cpapi_inbound_rate_limit_global_concurrency", "Configured global concurrency limit.", float64(limiter.GlobalConcurrency))
	writeGaugeMetric(&out, "cpapi_inbound_rate_limit_in_flight", "Current admitted in-flight requests.", float64(limiter.InFlight))
	writeGaugeMetric(&out, "cpapi_inbound_rate_limit_tracked_keys", "Current number of tracked key token buckets.", float64(limiter.TrackedKeys))

	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(out.String()))
}

func writeGaugeMetric(dst *strings.Builder, name, help string, value float64) {
	if dst == nil {
		return
	}
	dst.WriteString("# HELP ")
	dst.WriteString(name)
	dst.WriteByte(' ')
	dst.WriteString(help)
	dst.WriteByte('\n')
	dst.WriteString("# TYPE ")
	dst.WriteString(name)
	dst.WriteString(" gauge\n")
	dst.WriteString(name)
	dst.WriteByte(' ')
	dst.WriteString(fmt.Sprintf("%.6f", value))
	dst.WriteByte('\n')
}

func writeCounterMetric(dst *strings.Builder, name, help string, value float64) {
	if dst == nil {
		return
	}
	dst.WriteString("# HELP ")
	dst.WriteString(name)
	dst.WriteByte(' ')
	dst.WriteString(help)
	dst.WriteByte('\n')
	dst.WriteString("# TYPE ")
	dst.WriteString(name)
	dst.WriteString(" counter\n")
	dst.WriteString(name)
	dst.WriteByte(' ')
	dst.WriteString(fmt.Sprintf("%.6f", value))
	dst.WriteByte('\n')
}

func boolToFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
