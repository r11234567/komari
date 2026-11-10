//go:build linux

package trafficledger

// This opt-in harness exercises the production storage, traffic consumer and
// background worker in a separate process. HTTP/browser acceptance is covered
// by feature-e2e; these timings are storage calls, not HTTP request latency.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/metric"
	v1 "github.com/komari-monitor/komari/protocol/v1"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type resourceSamples struct {
	sync.Mutex
	latencies map[string][]float64
	errors    []string
	peakRSS   int64
	hot       []int64
}

func (s *resourceSamples) record(name string, began time.Time, err error) {
	s.Lock()
	defer s.Unlock()
	s.latencies[name] = append(s.latencies[name], float64(time.Since(began))/float64(time.Millisecond))
	if err != nil && len(s.errors) < 30 {
		s.errors = append(s.errors, name+": "+err.Error())
	}
}

func resourceRSS() int64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			return n * 1024
		}
	}
	return 0
}

func resourceCPU() time.Duration {
	var r syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r)
	return time.Duration(r.Utime.Nano() + r.Stime.Nano())
}

func resourcePercentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copy := append([]float64(nil), values...)
	sort.Float64s(copy)
	return copy[min(len(copy)-1, int(float64(len(copy)-1)*p))]
}

func TestTelemetryResourceAcceptance(t *testing.T) {
	source := os.Getenv("KOMARI_TELEMETRY_RESOURCE_SOURCE")
	if source == "" {
		t.Skip("requires an explicitly supplied migrated disposable incident fixture and cgroup limits")
	}
	output := os.Getenv("KOMARI_TELEMETRY_RESOURCE_OUTPUT")
	require.NotEmpty(t, output, "write the acceptance report outside the source tree")
	duration := 15 * time.Minute
	if value := os.Getenv("KOMARI_TELEMETRY_RESOURCE_DURATION"); value != "" {
		var err error
		duration, err = time.ParseDuration(value)
		require.NoError(t, err)
		require.GreaterOrEqual(t, duration, 30*time.Second)
	}
	// Verify actual kernel limits; GOMAXPROCS/GOMEMLIMIT alone are not limits.
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	require.NoError(t, err)
	rel := strings.TrimPrefix(strings.TrimSpace(string(cgroup)), "0::")
	cgroupDir := filepath.Join("/sys/fs/cgroup", rel)
	memoryMax, err := os.ReadFile(filepath.Join(cgroupDir, "memory.max"))
	require.NoError(t, err)
	memory, err := strconv.ParseInt(strings.TrimSpace(string(memoryMax)), 10, 64)
	require.NoError(t, err)
	require.LessOrEqual(t, memory, int64(1<<30))
	cpuMax, err := os.ReadFile(filepath.Join(cgroupDir, "cpu.max"))
	require.NoError(t, err)
	cpuFields := strings.Fields(string(cpuMax))
	quota, err := strconv.ParseInt(cpuFields[0], 10, 64)
	require.NoError(t, err)
	period, err := strconv.ParseInt(cpuFields[1], 10, 64)
	require.NoError(t, err)
	require.LessOrEqual(t, quota, period)

	ctx := context.Background()
	dir := t.TempDir()
	uri := url.URL{Scheme: "file", Path: filepath.Join(source, "metrics.db"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite3", uri.String())
	require.NoError(t, err)
	path := filepath.Join(dir, "metrics.db")
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", path)
	require.NoError(t, err)
	var originalPoints int64
	require.NoError(t, db.QueryRow("SELECT COALESCE(SUM(point_count),0) FROM metric_point_blocks").Scan(&originalPoints))
	require.Greater(t, originalPoints, int64(10_000_000))
	require.NoError(t, db.Close())
	archive := filepath.Join(dir, "archive")
	require.NoError(t, os.Mkdir(archive, 0700))
	files := 0
	require.NoError(t, filepath.WalkDir(filepath.Join(source, "archive"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("fixture archive contains a symlink")
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl.zst") {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join(source, "archive"), path)
		if err != nil {
			return err
		}
		target := filepath.Join(archive, rel)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		// Immutable files may be hardlinked: expiration only unlinks this copy.
		if err = os.Link(path, target); err != nil {
			return err
		}
		files++
		return nil
	}))
	// Use the real application's cgroup-aware cache/read-pool configuration.
	s, err := metricstore.OpenStore(ctx, &metricstore.MetricStoreConfig{Driver: "sqlite", DSN: path, ArchiveDirectory: archive})
	require.NoError(t, err)
	defer s.Close()
	// Preserve old incident timestamps under the actual current clock. This
	// changes only the disposable copy's policy, never time or source data.
	defs, err := s.ListMetrics(ctx)
	require.NoError(t, err)
	for _, def := range defs {
		_, err = s.UpdateMetricRetention(ctx, def.Name, 365)
		require.NoError(t, err)
	}
	require.NoError(t, s.RequireTelemetryMigration(ctx), "supply a fully migrated fixture")
	require.NoError(t, metricstore.EnsureBuiltinMetricDefinitions(ctx, s))
	metadataPath := filepath.Join(dir, "metadata.db")
	metadata, err := gorm.Open(sqlite.Open(metadataPath+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	metaSQL, err := metadata.DB()
	require.NoError(t, err)
	metaSQL.SetMaxOpenConns(1)
	defer metaSQL.Close()
	require.NoError(t, metadata.AutoMigrate(&models.Client{}, &models.TrafficReportNotification{}, &models.TrafficDailyLedger{}, &models.TrafficCalibrationAdjustment{}, &models.TrafficCycleFirstDay{}))
	require.NoError(t, metadata.AutoMigrate(TelemetryModels()...))
	for i := range 10 {
		require.NoError(t, metadata.Create(&models.Client{UUID: fmt.Sprintf("load-node-%02d", i), Token: fmt.Sprintf("fixture-%02d", i)}).Error)
	}
	config.SetDb(metadata)
	require.NoError(t, config.SetMany(map[string]any{"metric_archive_directory": archive, "metric_hot_window_minutes": 1, "metric_archive_segment_minutes": 1, "metric_archive_retention_days": 365}))
	restore := metricstore.SwapStoreForTest(s)
	defer restore()
	ConfigureTelemetry(metadata)
	metricstore.ConfigureTelemetryConsumer(ConsumeTelemetry)
	metricstore.ConfigureTelemetryLedgerGC(PruneTelemetrySlices)
	require.NoError(t, metricstore.ActivateTelemetry(ctx))
	var entity string
	var captured int64
	// This actual capture has memory/traffic/Ping history but no CPU raw rows;
	// query a real populated series instead of inventing historical CPU data.
	const historicalMetric = metricstore.MetricRAM
	require.NoError(t, metaSourceQuery(path, "SELECT s.entity_id,MAX(b.end_nano) FROM metric_point_blocks b JOIN metric_series s ON s.id=b.series_id WHERE s.metric_name='memory.used' GROUP BY s.entity_id LIMIT 1", &entity, &captured))

	samples := &resourceSamples{latencies: make(map[string][]float64)}
	initialRSS := resourceRSS()
	initialCPU := resourceCPU()
	began := time.Now()
	before, err := s.Telemetry().Diagnostics(ctx)
	require.NoError(t, err)
	run, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	metricstore.StartTelemetryScheduler()
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		require.NoError(t, metricstore.StopTelemetryScheduler(stop))
	}()
	var workers sync.WaitGroup
	launch := func(f func()) { workers.Add(1); go func() { defer workers.Done(); f() }() }
	// Ten nodes, each once per five seconds; submissions are evenly staggered.
	launch(func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		sequence := 0
		for {
			select {
			case <-run.Done():
				return
			case <-ticker.C:
			}
			sequence++
			r := v1.Report{UUID: fmt.Sprintf("load-node-%02d", sequence%10), UpdatedAt: time.Now().UTC(), CPU: v1.CPUReport{Usage: float64(sequence % 100)}, Ram: v1.RamReport{Total: 1 << 30, Used: 256 << 20}, Network: v1.NetworkReport{Up: 4096, Down: 8192, TotalUp: int64(sequence) * 4096, TotalDown: int64(sequence) * 8192}}
			writeCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
			writeCtx = metricstore.WithReportIdentity(writeCtx, fmt.Sprintf("resource/%d", sequence), "")
			start := time.Now()
			_, err := metricstore.WriteReport(writeCtx, r)
			samples.record("ingest", start, err)
			if sequence%20 == 0 {
				start = time.Now()
				_, err = metricstore.WriteReport(writeCtx, r)
				samples.record("retry", start, err)
			}
			done()
		}
	})
	for worker := range 2 {
		launch(func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			iteration := worker
			for {
				select {
				case <-run.Done():
					return
				case <-ticker.C:
				}
				iteration++
				days := []int{1, 7, 30}[iteration%3]
				end := time.Unix(0, captured).Truncate(time.Hour).Add(time.Hour)
				q := metric.AggregateQuery{Query: metric.Query{MetricName: historicalMetric, EntityID: entity, Start: end.Add(-time.Duration(days) * 24 * time.Hour), End: end.Add(-time.Nanosecond)}, Interval: time.Hour, Aggregation: metric.AggAvg, PreserveSeries: true}
				queryCtx, done := context.WithTimeout(metric.WithTelemetryEntities(context.Background(), []string{entity}), 5*time.Second)
				start := time.Now()
				result, err := s.Telemetry().Chart(queryCtx, q, time.Now().UTC())
				if err == nil && len(result.Points) == 0 {
					err = errors.New("incident chart returned no data")
				}
				samples.record(fmt.Sprintf("chart_%dd", days), start, err)
				done()
			}
		})
	}
	launch(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-run.Done():
				return
			case <-ticker.C:
			}
			rss := resourceRSS()
			d, e := s.Telemetry().Diagnostics(run)
			samples.Lock()
			samples.peakRSS = max(samples.peakRSS, rss)
			if e == nil {
				samples.hot = append(samples.hot, d.HotBudgetBytes)
			}
			samples.Unlock()
		}
	})
	// Include a cold CSV source read, its cancellation, and a full snapshot
	// while foreground ingestion continues. These operations are not mocked.
	launch(func() {
		timer := time.NewTimer(duration / 2)
		defer timer.Stop()
		select {
		case <-run.Done():
			return
		case <-timer.C:
		}
		end := time.Unix(0, captured).Truncate(time.Hour).Add(time.Hour)
		q := metric.Query{MetricName: historicalMetric, EntityID: entity, Start: end.Add(-time.Hour), End: end.Add(-time.Nanosecond)}
		exportCtx, done := context.WithTimeout(metric.WithTelemetryEntities(context.Background(), []string{entity}), time.Minute)
		start := time.Now()
		_, err := s.Telemetry().ExportRawBatch(exportCtx, []metric.Query{q})
		samples.record("cold_export", start, err)
		done()
		cancelCtx, stop := context.WithCancel(metric.WithTelemetryEntities(context.Background(), []string{entity}))
		stop()
		start = time.Now()
		_, err = s.Telemetry().ExportRawBatch(cancelCtx, []metric.Query{q})
		if !errors.Is(err, context.Canceled) {
			samples.record("cancel_error", start, fmt.Errorf("expected cancellation, got %v", err))
		} else {
			samples.record("cancel", start, nil)
		}
		backup := filepath.Join(dir, "backup")
		if err = os.Mkdir(backup, 0700); err != nil {
			samples.record("backup", time.Now(), err)
			return
		}
		backupCtx, finish := context.WithTimeout(context.Background(), 2*time.Minute)
		start = time.Now()
		err = metricstore.BackupFull(backupCtx, backup, func() error {
			_, e := metaSQL.ExecContext(backupCtx, "VACUUM INTO ?", filepath.Join(backup, "metadata.db"))
			return e
		})
		samples.record("backup", start, err)
		finish()
	})
	workers.Wait()
	elapsed := time.Since(began)
	cpu := float64(resourceCPU()-initialCPU) / float64(elapsed)
	after, err := s.Telemetry().Diagnostics(context.Background())
	require.NoError(t, err)
	summaries := make(map[string]any)
	for name, values := range samples.latencies {
		summaries[name] = map[string]any{"count": len(values), "p95_ms": resourcePercentile(values, .95), "p99_ms": resourcePercentile(values, .99), "max_ms": resourcePercentile(values, 1)}
	}
	cpuInfo, _ := os.ReadFile("/proc/cpuinfo")
	procStat, _ := os.ReadFile("/proc/stat")
	report := map[string]any{"go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "duration_seconds": elapsed.Seconds(), "cgroup_cpu_max": strings.TrimSpace(string(cpuMax)), "cgroup_memory_max": memory, "cpu_average_cores": cpu, "initial_rss_bytes": initialRSS, "peak_rss_bytes": samples.peakRSS, "rss_increase_bytes": samples.peakRSS - initialRSS, "original_logical_points": originalPoints, "immutable_archive_files": files, "nodes": 10, "report_interval_seconds_per_node": 5, "chart_workers": 2, "chart_interval_seconds_per_worker": 5, "timings": summaries, "errors": samples.errors, "initial_backlog": before, "final_backlog": after, "hot_bytes_samples": samples.hot, "cpuinfo": string(cpuInfo), "proc_stat_end": string(procStat), "limitations": "WSL Linux cgroup storage benchmark; not Debian Docker or HTTP latency. RSS increase includes mixed queries/export/backup, not isolated archive RSS. Setup/migration excluded from measured CPU."}
	bytes, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(output, bytes, 0600))
	t.Logf("Resource report %s: %.3f cores, RSS %d MiB, ingest p95 %.2fms / p99 %.2fms", output, cpu, samples.peakRSS>>20, resourcePercentile(samples.latencies["ingest"], .95), resourcePercentile(samples.latencies["ingest"], .99))
	require.Empty(t, samples.errors)
	require.NotEmpty(t, samples.latencies["chart_30d"])
	require.Less(t, resourcePercentile(samples.latencies["ingest"], .95), 500.0)
	require.Less(t, resourcePercentile(samples.latencies["ingest"], .99), 2000.0)
	for _, days := range []int{1, 7, 30} {
		require.Less(t, resourcePercentile(samples.latencies[fmt.Sprintf("chart_%dd", days)], .95), 2000.0)
	}
	require.LessOrEqual(t, samples.peakRSS, int64(700<<20))
	if duration >= 15*time.Minute {
		require.LessOrEqual(t, cpu, .45)
	}
}

func metaSourceQuery(path, query string, values ...any) error {
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return err
	}
	defer db.Close()
	return db.QueryRow(query).Scan(values...)
}
