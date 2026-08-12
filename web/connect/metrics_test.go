package connectapi

import (
	"math"
	"testing"
	"time"

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
