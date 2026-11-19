package connectapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/metricstore"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	"google.golang.org/protobuf/proto"
)

func gpuWireFixture(legacy bool) []*metricsv1.MetricsPoint {
	points := completeMetricPoints()
	names := []string{metricstore.MetricGPUDeviceUsage, metricstore.MetricGPUMem, metricstore.MetricGPUMemTotal, metricstore.MetricGPUTemp}
	indexKey, nameKey := "device_index", "device_name"
	if legacy {
		names = []string{"gpu.utilization_percent", "gpu.memory_used_bytes", "gpu.memory_total_bytes", "gpu.temperature_celsius"}
		indexKey, nameKey = "id", "name"
	}
	for index, id := range []string{"10", "2"} {
		values := []float64{25 + float64(index)*50, float64(1<<30) * float64(index+1), 8 << 30, 50 + float64(index)*10}
		for field, name := range names {
			points = append(points, &metricsv1.MetricsPoint{
				Metric: name, Value: values[field], ObservedAt: points[0].ObservedAt,
				Labels: map[string]string{indexKey: id, nameKey: "GPU " + id},
			})
		}
	}
	return points
}

func TestGPUMetricWireCompatibility(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		report, err := metricPointsToReport(gpuWireFixture(legacy))
		if err != nil {
			t.Fatalf("legacy=%v: %v", legacy, err)
		}
		if report.GPU == nil || report.GPU.Count != 2 || report.GPU.AverageUsage != 50 {
			t.Fatalf("GPU aggregate lost: %+v", report.GPU)
		}
		first := report.GPU.DetailedInfo[0]
		if first.Name != "GPU 2" || first.Utilization != 75 || first.MemoryUsed != 2<<30 || first.MemoryTotal != 8<<30 || first.Temperature != 60 {
			t.Fatalf("GPU identity, numeric ordering or counters lost: %+v", first)
		}
	}
}

func TestGPUMetricAliasesRejectAmbiguousAndUnknownData(t *testing.T) {
	cases := map[string]func([]*metricsv1.MetricsPoint) []*metricsv1.MetricsPoint{
		"conflicting id": func(points []*metricsv1.MetricsPoint) []*metricsv1.MetricsPoint {
			points[18].Labels["device_index"] = "other"
			return points
		},
		"conflicting name": func(points []*metricsv1.MetricsPoint) []*metricsv1.MetricsPoint {
			points[18].Labels["device_name"] = "other"
			return points
		},
		"missing identity": func(points []*metricsv1.MetricsPoint) []*metricsv1.MetricsPoint {
			points[18].Labels = nil
			return points
		},
		"duplicate alias": func(points []*metricsv1.MetricsPoint) []*metricsv1.MetricsPoint {
			duplicate := proto.Clone(points[18]).(*metricsv1.MetricsPoint)
			duplicate.Metric = metricstore.MetricGPUDeviceUsage
			duplicate.Labels = map[string]string{"device_index": "10", "device_name": "GPU 10"}
			return append(points, duplicate)
		},
		"unknown metric": func(points []*metricsv1.MetricsPoint) []*metricsv1.MetricsPoint {
			points[18].Metric = "gpu.unknown"
			return points
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := metricPointsToReport(mutate(gpuWireFixture(true))); err == nil {
				t.Fatal("ambiguous or unknown GPU data accepted")
			}
		})
	}
}

func TestDualEnd_GPUCompatibilityKeepsAuthenticatedMetricsAccepted(t *testing.T) {
	client := metricsv1connect.NewMetricsServiceClient(newDPoPTestClient(testAccessToken, testAgentPrivKey), testServer.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, legacy := range []bool{true, false} {
		state := metricSequenceFor(testClientUUID)
		state.Lock()
		sequence := state.accepted + 1
		state.Unlock()
		points := gpuWireFixture(legacy)
		response, err := client.SubmitMetrics(ctx, connect.NewRequest(&metricsv1.SubmitMetricsRequest{
			AgentId: testClientUUID, Sequence: sequence, Points: points,
		}))
		if err != nil {
			t.Fatalf("GPU metrics interrupted authenticated transport, legacy=%v: %v", legacy, err)
		}
		if response.Msg.AcceptedSequence != sequence || response.Msg.AcceptedPoints != uint32(len(points)) {
			t.Fatal("GPU batch was not fully acknowledged")
		}
		report := agentRuntimeReports()[testClientUUID]
		if report == nil || report.GPU == nil || report.GPU.Count != 2 || report.GPU.AverageUsage != 50 {
			t.Fatalf("accepted GPU data missing from live report: %+v", report)
		}
	}
}
