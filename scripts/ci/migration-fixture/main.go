package main

// Synthetic legacy history for the disposable upgrade fixture. Credentials
// come only from the real HTTPS installation backup; they are never uploaded.
import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
)

type migrationScope struct {
	AgentID    string `json:"agentId"`
	PingTaskID uint32 `json:"pingTaskId"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	root := os.Getenv("E2E_ROOT")
	if exec.Command("bash", "scripts/ci/e2e/guard.sh").Run() != nil || root != filepath.Join(os.Getenv("RUNNER_TEMP"), "komari-e2e") {
		return fmt.Errorf("disposable hosted fixture only")
	}
	dir := filepath.Join(root, "migration-panel", "data")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	bundle, err := zip.OpenReader(filepath.Join(root, "real-full-backup.zip"))
	if err != nil {
		return err
	}
	defer bundle.Close()
	for _, f := range bundle.File {
		if f.Name != "komari.db" && f.Name != "secret.key" {
			continue
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(dir, f.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		ce := out.Close()
		if err != nil {
			return err
		}
		if ce != nil {
			return ce
		}
	}
	// The installation test deliberately uses a one-minute online window to
	// exercise archive retirement quickly. This fixture tests the default live
	// contract after migration instead, including a full 14.5-minute reload.
	metadata, err := sql.Open("sqlite3", filepath.Join(dir, "komari.db"))
	if err != nil {
		return err
	}
	for key, value := range map[string]string{"metric_hot_window_minutes": "15", "metric_archive_segment_minutes": "1440"} {
		if _, err = metadata.Exec("INSERT INTO configs (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			metadata.Close()
			return err
		}
	}
	if err = metadata.Close(); err != nil {
		return err
	}
	var scope migrationScope
	data, err := os.ReadFile(filepath.Join(root, "qualification-scope.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &scope); err != nil {
		return err
	}
	if scope.AgentID == "" {
		return fmt.Errorf("missing actually enrolled Agent")
	}
	ctx := context.Background()
	s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	if err != nil {
		return err
	}
	defer s.Close()
	return seedHistory(ctx, s, scope, root, time.Now().UTC().Add(-time.Minute).Truncate(time.Minute))
}

func seedHistory(ctx context.Context, s *metric.Store, scope migrationScope, root string, cutoff time.Time) error {
	var err error
	if err = metricstore.EnsureBuiltinMetricDefinitions(ctx, s); err != nil {
		return err
	}
	if _, err = s.UpdateMetricRetention(ctx, metricstore.MetricCPU, 400); err != nil {
		return err
	}
	if scope.PingTaskID == 0 {
		return fmt.Errorf("missing synthetic runner ping task")
	}
	if _, err = s.UpdateMetricRetention(ctx, metricstore.MetricPingLatency, 400); err != nil {
		return err
	}
	start := cutoff.Add(-24 * time.Hour)
	var batch []metric.Point
	var cpu []metric.Point
	var ping []metric.Point
	pingTags := map[string]string{"task_id": strconv.FormatUint(uint64(scope.PingTaskID), 10)}
	pingPoint := func(at time.Time, i int) metric.Point {
		value := float64(10 + i%71)
		if i%13 == 0 {
			value = -1
		}
		return metric.Point{MetricName: metricstore.MetricPingLatency, EntityID: scope.AgentID, Tags: pingTags, Timestamp: at, Value: value}
	}
	for i := 0; i < 8640; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		for name, value := range map[string]float64{metricstore.MetricCPU: float64(i % 100), metricstore.MetricNetTotalUp: float64(1000000 + i*100), metricstore.MetricNetTotalDown: float64(2000000 + i*200), metricstore.MetricTrafficUp: 100, metricstore.MetricTrafficDown: 200} {
			p := metric.Point{MetricName: name, EntityID: scope.AgentID, Timestamp: at, Value: value}
			batch = append(batch, p)
			if name == metricstore.MetricCPU {
				cpu = append(cpu, p)
			}
		}
		p := pingPoint(at, i)
		batch = append(batch, p)
		ping = append(ping, p)
		if len(batch) >= 1000 {
			if err = s.WriteBatch(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err = s.WriteBatch(ctx, batch); err != nil {
			return err
		}
	}
	// This history is generated from formulas into production-created tables.
	// No local database, DDL dump or operator row is an input to this program.
	batch = nil
	historicalPoints := 0
	for i := 48; i < 400*48; i++ {
		// Populate every retained chart cell without manufacturing hundreds
		// of thousands of redundant observations in the year-old fixture.
		stride := 1
		switch {
		case i >= 365*48:
			stride = 48
		case i >= 180*48:
			stride = 24
		case i >= 60*48:
			stride = 12
		case i >= 15*48:
			stride = 4
		case i >= 7*48:
			stride = 2
		}
		if i%stride != 0 {
			continue
		}
		historicalPoints++
		p := metric.Point{MetricName: metricstore.MetricCPU, EntityID: scope.AgentID, Timestamp: cutoff.Add(-time.Duration(i)*30*time.Minute - time.Second), Value: float64((i * 17) % 101)}
		cpu = append(cpu, p)
		batch = append(batch, p)
		pp := pingPoint(p.Timestamp, i)
		ping = append(ping, pp)
		batch = append(batch, pp)
		if len(batch) >= 500 {
			if err = s.WriteBatch(ctx, batch); err != nil {
				return err
			}
			batch = nil
		}
	}
	if len(batch) > 0 {
		if err = s.WriteBatch(ctx, batch); err != nil {
			return err
		}
	}
	var windows []map[string]any
	hoursList := []int{1, 6, 12, 24, 168, 360}
	for day := 30; day <= 390; day += 15 {
		hoursList = append(hoursList, day*24)
	}
	hoursList = append(hoursList, 365*24, 400*24)
	for _, hours := range hoursList {
		span := time.Duration(hours) * time.Hour
		interval := metric.TelemetryChartInterval(span, 600)
		end := metric.TelemetryBucketStart(cutoff, interval)
		q := metric.AggregateQuery{Query: metric.Query{MetricName: metricstore.MetricCPU, EntityID: scope.AgentID, Start: end.Add(-span), End: end.Add(-time.Nanosecond)}, Interval: interval, Aggregation: metric.AggAvg, PreserveSeries: true}
		var selected []metric.Point
		for _, p := range cpu {
			if !p.Timestamp.Before(q.Start) && !p.Timestamp.After(q.End) {
				selected = append(selected, p)
			}
		}
		points, err := metric.AggregatePoints(selected, q)
		if err != nil {
			return err
		}
		windows = append(windows, map[string]any{"hours": hours, "start": q.Start, "end": q.End, "interval_seconds": interval.Seconds(), "points": points})
	}
	oracle, err := json.MarshalIndent(map[string]any{"origin": "formula-generated synthetic observations only", "windows": windows, "cpu_observations": cpu, "ping_observations": ping, "ping_task_id": scope.PingTaskID}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(root, "migration-curve-oracle.json"), oracle, 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "migration-manifest.json"), []byte(fmt.Sprintf("{\"synthetic_points\":%d,\"agentId\":%q}", 6*8640+2*historicalPoints, scope.AgentID)), 0600)
}
