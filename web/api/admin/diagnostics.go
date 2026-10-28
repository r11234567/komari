package admin

import (
	"runtime/metrics"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	"github.com/komari-monitor/komari/internal/diagnostics"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/workspace"
)

// diagnosticRuntime samples inexpensive runtime counters on explicit admin
// requests only. It enables neither block/mutex profiling nor runtime tracing.
func diagnosticRuntime() map[string]any {
	names := []string{"/cpu/classes/user:cpu-seconds", "/cpu/classes/gc/total:cpu-seconds", "/cpu/classes/scavenge/total:cpu-seconds", "/sched/goroutines:goroutines", "/memory/classes/heap/objects:bytes", "/memory/classes/total:bytes", "/gc/cycles/total:gc-cycles"}
	samples := make([]metrics.Sample, len(names))
	for i, name := range names {
		samples[i].Name = name
	}
	metrics.Read(samples)
	out := make(map[string]any, len(names))
	for _, sample := range samples {
		switch sample.Value.Kind() {
		case metrics.KindUint64:
			out[sample.Name] = sample.Value.Uint64()
		case metrics.KindFloat64:
			out[sample.Name] = sample.Value.Float64()
		}
	}
	return out
}

func diagnosticsHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	metricPools := map[string]any{}
	if store := metricstore.GetStore(); store != nil {
		for name, s := range store.PoolStats() {
			metricPools[name] = diagnostics.PoolSnapshot{MaxOpen: s.MaxOpenConnections, Open: s.OpenConnections, InUse: s.InUse, Idle: s.Idle, WaitCount: s.WaitCount, WaitDurationNS: int64(s.WaitDuration)}
		}
	}
	api.RespondSuccess(c, gin.H{
		"schema_version": 1, "collected_at": time.Now().UTC(),
		"requests": diagnostics.Default.Snapshot(), "runtime": diagnosticRuntime(),
		"database":     gin.H{"primary_pool": dbcore.DiagnosticPool(), "primary_queries": diagnostics.PrimaryQueries.Snapshot(), "metric_pools": metricPools},
		"report_queue": metricstore.ReportQueueDiagnostics(),
		"workspace":    workspace.Default.DiagnosticCounts(), "verification": credentialgateway.Default.DiagnosticCounts(),
	})
}
