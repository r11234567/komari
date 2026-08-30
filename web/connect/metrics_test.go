package connectapi

import (
	"math"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func completeMetricPoints() []*metricsv1.MetricsPoint {
	now := timestamppb.New(time.Now().UTC())
	names := []string{"cpu.usage_percent", "memory.total_bytes", "memory.used_bytes", "swap.total_bytes", "swap.used_bytes", "load.1", "load.5", "load.15", "disk.total_bytes", "disk.used_bytes", "network.up_bytes_per_second", "network.down_bytes_per_second", "network.total_up_bytes", "network.total_down_bytes", "connections.tcp", "connections.udp", "system.uptime_seconds", "system.process_count"}
	points := make([]*metricsv1.MetricsPoint, 0, len(names))
	for index, name := range names {
		points = append(points, &metricsv1.MetricsPoint{Metric: name, Value: float64(index + 1), ObservedAt: now})
	}
	return points
}

func TestLegacyReportMetricPointsUsePublicMetricNames(t *testing.T) {
	report := &legacyv1.Report{UpdatedAt: time.Now().UTC()}
	report.CPU.Usage = 42
	report.Network.Down = 1024
	points := legacyReportMetricPoints(report)
	values := make(map[string]float64, len(points))
	for _, point := range points {
		values[point.Metric] = point.Value
	}
	if values[metricstore.MetricCPU] != 42 || values[metricstore.MetricNetIn] != 1024 {
		t.Fatalf("unexpected live metric mapping: %#v", values)
	}
}

func TestMetricPointsToReportPreservesLegacySurface(t *testing.T) {
	report, err := metricPointsToReport(completeMetricPoints())
	if err != nil {
		t.Fatal(err)
	}
	if report.CPU.Usage != 1 || report.Network.Up != 11 || report.Connections.TCP != 15 || report.Process != 18 {
		t.Fatalf("unexpected report mapping: %+v", report)
	}
}

func TestMetricPointsToReportRejectsUnknownAndMissing(t *testing.T) {
	points := completeMetricPoints()
	points[0].Metric = "unknown.metric"
	if _, err := metricPointsToReport(points); err == nil {
		t.Fatal("expected unknown metric rejection")
	}
	if _, err := metricPointsToReport(completeMetricPoints()[:2]); err == nil {
		t.Fatal("expected incomplete batch rejection")
	}
	points = completeMetricPoints()
	points[0].Value = math.Inf(1)
	if _, err := metricPointsToReport(points); err == nil {
		t.Fatal("expected non-finite value rejection")
	}
}

func TestConnectMetricBatchResultsSplitByEntity(t *testing.T) {
	aggregate := connectAggregatePointsForEntity([]metric.AggregatePoint{
		{EntityID: "node-a", Value: 1},
		{EntityID: "node-b", Value: 2},
		{EntityID: "node-a", Value: 3},
	}, "node-a")
	if len(aggregate) != 2 || aggregate[0].Value != 1 || aggregate[1].Value != 3 {
		t.Fatalf("unexpected aggregate split: %#v", aggregate)
	}

	raw := connectRawPointsForEntity([]metric.Point{
		{EntityID: "node-b", Value: 4},
		{EntityID: "node-a", Value: 5},
	}, "node-a")
	if len(raw) != 1 || raw[0].Value != 5 {
		t.Fatalf("unexpected raw split: %#v", raw)
	}
}
