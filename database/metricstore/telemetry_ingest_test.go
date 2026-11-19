package metricstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
	v1 "github.com/komari-monitor/komari/protocol/v1"
)

func TestTelemetryReportACKSurvivesRestartAndSequenceReset(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	path := filepath.Join(dir, "metrics.db")
	open := func() *metric.Store {
		s, err := metric.Open(ctx, metric.SQLite(path, metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
		if err != nil {
			t.Fatal(err)
		}
		if err = EnsureBuiltinMetricDefinitions(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err = s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	restore := SwapStoreForTest(s)
	defer restore()
	defer func() { s.Close() }()
	StartReportBatcher()
	defer StopReportBatcher(ctx)
	now := time.Now().UTC()
	report := v1.Report{UUID: "node", UpdatedAt: now}
	report.Network.TotalUp = 1 << 54
	report.Network.TotalDown = 1 << 54
	writeCtx := WithReportIdentity(ctx, "metrics/1/"+now.Format(time.RFC3339Nano))
	if _, err := WriteReport(writeCtx, report); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Telemetry().Diagnostics(ctx); err != nil || d.Accepted != 1 {
		t.Fatalf("ACK was only queued %+v %v", d, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	SwapStoreForTest(s)
	if _, err := WriteReport(writeCtx, report); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Telemetry().Diagnostics(ctx); err != nil || d.Accepted != 1 {
		t.Fatalf("retry duplicated after restart %+v %v", d, err)
	}
	report.CPU.Usage = 9
	if _, err := WriteReport(writeCtx, report); err != nil {
		t.Fatalf("altered retry %v", err)
	}
	report.UpdatedAt = now.Add(time.Second)
	if _, err := WriteReport(WithReportIdentity(ctx, "metrics/1/"+report.UpdatedAt.Format(time.RFC3339Nano)), report); err != nil {
		t.Fatalf("Agent sequence restart lost sample: %v", err)
	}
	events := 0
	if n, err := s.Telemetry().DeliverTraffic(ctx, 256, func(_ context.Context, e metric.TelemetryTrafficEvent) error {
		events++
		if e.Traffic.TotalUp != 1<<54 || e.StoreID == "" {
			t.Fatalf("integer traffic lost %+v", e)
		}
		return nil
	}); err != nil || n != 2 || events != 2 {
		t.Fatal(n, events, err)
	}
	if q := ReportQueueDiagnostics(); q.Accepted != 0 || q.Depth != 0 {
		t.Fatalf("durable path used asynchronous queue %+v", q)
	}
}
