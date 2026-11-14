package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
)

// Opt-in runs the exact hosted generator and production migration locally.
// This is a database contract; browser screenshots remain a hosted E2E check.
func TestSyntheticMigrationCurveFixture(t *testing.T) {
	if os.Getenv("KOMARI_TELEMETRY_MIGRATION_CURVE_FIXTURE") != "1" {
		t.Skip("explicit synthetic database qualification")
	}
	ctx := context.Background()
	root := t.TempDir()
	s, err := metric.Open(ctx, metric.SQLite(filepath.Join(root, "metrics.db"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cutoff := time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)
	scope := migrationScope{AgentID: "synthetic-node", PingTaskID: 1}
	if err = seedHistory(ctx, s, scope, root, cutoff); err != nil {
		t.Fatal(err)
	}
	if err = s.MigrateLegacy(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(root, "archive"), RawRetention: 24 * time.Hour}, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(root, "archive"), RawRetention: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := s.Telemetry().Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.HotBudgetBytes != 0 || diagnostics.HotLimitBytes != 16<<20 {
		t.Fatalf("migration retained archived raw SQL or changed the default budget: %+v", diagnostics)
	}
	var source struct {
		CPU     []metric.Point `json:"cpu_observations"`
		Ping    []metric.Point `json:"ping_observations"`
		Windows []struct {
			Hours    int `json:"hours"`
			Interval int `json:"interval_seconds"`
		} `json:"windows"`
	}
	b, err := os.ReadFile(filepath.Join(root, "migration-curve-oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &source); err != nil {
		t.Fatal(err)
	}
	for _, w := range source.Windows {
		i := time.Duration(w.Interval) * time.Second
		now := time.Now().UTC()
		start := metric.TelemetryBucketStart(now.Add(-time.Duration(w.Hours)*time.Hour), i).Add(i)
		for _, raw := range [][]metric.Point{source.CPU, source.Ping} {
			q := metric.AggregateQuery{Query: metric.Query{MetricName: raw[0].MetricName, EntityID: scope.AgentID, Start: start, End: now}, Interval: i, Aggregation: metric.AggAvg, PreserveSeries: true}
			var selected []metric.Point
			for _, p := range raw {
				if !p.Timestamp.Before(start) && !p.Timestamp.After(now) {
					// Negative Ping observations are failed attempts, not latency
					// samples. Match the independent browser oracle's valid average.
					if p.MetricName == metricstore.MetricPingLatency && p.Value < 0 {
						continue
					}
					selected = append(selected, p)
				}
			}
			want, err := metric.AggregatePoints(selected, q)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Telemetry().Chart(ctx, q, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Gaps) > 0 || len(got.Points) != len(want) {
				t.Fatalf("%s %dh coverage gaps=%d got=%d want=%d", q.MetricName, w.Hours, len(got.Gaps), len(got.Points), len(want))
			}
			for n, p := range got.Points {
				if !p.Bucket.Equal(want[n].Bucket) || p.Count != want[n].Count || math.Abs(p.Value-want[n].Value) > 1e-7 {
					t.Fatalf("%s %dh point %d: got %+v want %+v", q.MetricName, w.Hours, n, p, want[n])
				}
			}
		}
	}
	if d, err := s.GetMetric(ctx, metricstore.MetricCPU); err != nil || d.RetentionDays != 400 {
		t.Fatal("fixture retention", d, err)
	}
}
