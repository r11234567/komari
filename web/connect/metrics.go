package connectapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/rpc"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	clientapi "github.com/komari-monitor/komari/web/api/client"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
)

const maxMetricPointsPerBatch = 256

type metricsService struct {
	metricsv1connect.UnimplementedMetricsServiceHandler
}

func (s *metricsService) SubmitMetrics(ctx context.Context, req *connect.Request[metricsv1.SubmitMetricsRequest]) (*connect.Response[metricsv1.SubmitMetricsResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if req.Msg.Sequence == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("metrics sequence is required"))
	}
	if len(req.Msg.Points) == 0 || len(req.Msg.Points) > maxMetricPointsPerBatch {
		return nil, connectError(connect.CodeInvalidArgument, fmt.Errorf("metrics batch must contain between 1 and %d points", maxMetricPointsPerBatch))
	}
	report, err := metricPointsToReport(req.Msg.Points)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	if err := clientapi.IngestReportContext(ctx, agentID, report, 3, true); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&metricsv1.SubmitMetricsResponse{AcceptedSequence: req.Msg.Sequence, AcceptedPoints: uint32(len(req.Msg.Points))}), nil
}

func metricPointsToReport(points []*metricsv1.MetricsPoint) (legacyv1.Report, error) {
	report := legacyv1.Report{}
	seen := make(map[string]struct{}, len(points))
	gpus := make(map[string]*legacyv1.GPUDeviceInfo)
	for _, point := range points {
		if point == nil || point.ObservedAt == nil || !point.ObservedAt.IsValid() {
			return report, errors.New("every metric point requires a valid observed_at")
		}
		if math.IsNaN(point.Value) || math.IsInf(point.Value, 0) || point.Value < 0 {
			return report, fmt.Errorf("metric %q contains an invalid value", point.Metric)
		}
		if point.Value > math.MaxInt64 {
			return report, fmt.Errorf("metric %q exceeds the canonical integer range", point.Metric)
		}
		isGPU := len(point.Metric) > 4 && point.Metric[:4] == "gpu."
		if _, duplicate := seen[point.Metric]; duplicate && !isGPU {
			return report, fmt.Errorf("duplicate aggregate metric %q", point.Metric)
		}
		if !isGPU {
			seen[point.Metric] = struct{}{}
		}
		value := point.Value
		switch point.Metric {
		case "cpu.usage_percent":
			report.CPU.Usage = value
		case "memory.total_bytes":
			report.Ram.Total = int64(value)
		case "memory.used_bytes":
			report.Ram.Used = int64(value)
		case "swap.total_bytes":
			report.Swap.Total = int64(value)
		case "swap.used_bytes":
			report.Swap.Used = int64(value)
		case "load.1":
			report.Load.Load1 = value
		case "load.5":
			report.Load.Load5 = value
		case "load.15":
			report.Load.Load15 = value
		case "disk.total_bytes":
			report.Disk.Total = int64(value)
		case "disk.used_bytes":
			report.Disk.Used = int64(value)
		case "network.up_bytes_per_second":
			report.Network.Up = int64(value)
		case "network.down_bytes_per_second":
			report.Network.Down = int64(value)
		case "network.total_up_bytes":
			report.Network.TotalUp = int64(value)
		case "network.total_down_bytes":
			report.Network.TotalDown = int64(value)
		case "connections.tcp":
			report.Connections.TCP = int(value)
		case "connections.udp":
			report.Connections.UDP = int(value)
		case "system.uptime_seconds":
			report.Uptime = int64(value)
		case "system.process_count":
			report.Process = int(value)
		case "gpu.utilization_percent", "gpu.memory_used_bytes", "gpu.memory_total_bytes", "gpu.temperature_celsius":
			id := point.Labels["id"]
			if id == "" || point.Labels["name"] == "" {
				return report, fmt.Errorf("metric %q requires GPU id and name labels", point.Metric)
			}
			gpu := gpus[id]
			if gpu == nil {
				gpu = &legacyv1.GPUDeviceInfo{Name: point.Labels["name"]}
				gpus[id] = gpu
			} else if gpu.Name != point.Labels["name"] {
				return report, fmt.Errorf("GPU %q has inconsistent name labels", id)
			}
			switch point.Metric {
			case "gpu.utilization_percent":
				gpu.Utilization = value
			case "gpu.memory_used_bytes":
				gpu.MemoryUsed = int64(value)
			case "gpu.memory_total_bytes":
				gpu.MemoryTotal = int64(value)
			case "gpu.temperature_celsius":
				gpu.Temperature = int(value)
			}
		default:
			return report, fmt.Errorf("unknown aggregate metric %q", point.Metric)
		}
		if point.ObservedAt.AsTime().After(report.UpdatedAt) {
			report.UpdatedAt = point.ObservedAt.AsTime()
		}
	}
	if len(gpus) > 0 {
		ids := make([]string, 0, len(gpus))
		for id := range gpus {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			left, leftErr := strconv.Atoi(ids[i])
			right, rightErr := strconv.Atoi(ids[j])
			if leftErr == nil && rightErr == nil {
				return left < right
			}
			return ids[i] < ids[j]
		})
		report.GPU = &legacyv1.GPUDetailReport{Count: len(ids)}
		for _, id := range ids {
			report.GPU.DetailedInfo = append(report.GPU.DetailedInfo, *gpus[id])
			report.GPU.AverageUsage += gpus[id].Utilization
		}
		report.GPU.AverageUsage /= float64(len(ids))
	}
	required := []string{"cpu.usage_percent", "memory.total_bytes", "memory.used_bytes", "swap.total_bytes", "swap.used_bytes", "load.1", "load.5", "load.15", "disk.total_bytes", "disk.used_bytes", "network.up_bytes_per_second", "network.down_bytes_per_second", "network.total_up_bytes", "network.total_down_bytes", "connections.tcp", "connections.udp", "system.uptime_seconds", "system.process_count"}
	for _, metric := range required {
		if _, ok := seen[metric]; !ok {
			return report, fmt.Errorf("required metric %q is missing", metric)
		}
	}
	if report.UpdatedAt.IsZero() {
		report.UpdatedAt = time.Now().UTC()
	}
	return report, nil
}
