package connectapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/rpc"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	clientapi "github.com/komari-monitor/komari/web/api/client"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxMetricPointsPerBatch  = 256
	maxMetricUploadBatches   = 512
	maxMetricUploadPoints    = 64 << 10
	maxMetricWatchAgents     = 256
	maxMetricWatchNames      = 64
	defaultMetricWatchPeriod = 2 * time.Second
	minimumMetricWatchPeriod = 500 * time.Millisecond
	maximumMetricWatchPeriod = time.Minute
)

type metricsService struct {
	metricsv1connect.UnimplementedMetricsServiceHandler
}

type metricAgentSequence struct {
	sync.Mutex
	accepted uint64
}

var metricSequences = struct {
	sync.Mutex
	agents map[string]*metricAgentSequence
}{agents: make(map[string]*metricAgentSequence)}

func metricSequenceFor(agentID string) *metricAgentSequence {
	metricSequences.Lock()
	defer metricSequences.Unlock()
	state := metricSequences.agents[agentID]
	if state == nil {
		state = &metricAgentSequence{}
		metricSequences.agents[agentID] = state
	}
	return state
}

func (s *metricsService) SubmitMetrics(ctx context.Context, req *connect.Request[metricsv1.SubmitMetricsRequest]) (*connect.Response[metricsv1.SubmitMetricsResponse], error) {
	response, err := ingestMetricBatch(ctx, rpc.MetaFromContext(ctx), req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

func ingestMetricBatch(ctx context.Context, meta *rpc.ContextMeta, batch *metricsv1.SubmitMetricsRequest) (*metricsv1.SubmitMetricsResponse, error) {
	if batch == nil {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("metrics batch is required"))
	}
	agentID, err := requireAgent(meta, batch.AgentId)
	if err != nil {
		return nil, err
	}
	// Every authenticated metrics submission is a heartbeat. Refresh presence
	// before sequence de-duplication so a retried batch also keeps the Agent
	// online while its previous acknowledgement is in doubt.
	clientapi.TouchConnectPresence(agentID)
	if batch.Sequence == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("metrics sequence is required"))
	}
	if len(batch.Points) == 0 || len(batch.Points) > maxMetricPointsPerBatch {
		return nil, connectError(connect.CodeInvalidArgument, fmt.Errorf("metrics batch must contain between 1 and %d points", maxMetricPointsPerBatch))
	}
	sequence := metricSequenceFor(agentID)
	sequence.Lock()
	defer sequence.Unlock()
	if batch.Sequence <= sequence.accepted {
		return &metricsv1.SubmitMetricsResponse{AcceptedSequence: sequence.accepted, AcceptedPoints: uint32(len(batch.Points))}, nil
	}
	report, err := metricPointsToReport(batch.Points)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	if err := clientapi.IngestReportContext(ctx, agentID, report, 3, true); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	sequence.accepted = batch.Sequence
	return &metricsv1.SubmitMetricsResponse{AcceptedSequence: batch.Sequence, AcceptedPoints: uint32(len(batch.Points))}, nil
}

func (s *metricsService) UploadMetrics(ctx context.Context, stream *connect.ClientStream[metricsv1.UploadMetricsRequest]) (*connect.Response[metricsv1.UploadMetricsResponse], error) {
	var acceptedSequence, acceptedPoints uint64
	var batches int
	for stream.Receive() {
		batches++
		if batches > maxMetricUploadBatches {
			return nil, connectError(connect.CodeResourceExhausted, errors.New("metrics upload contains too many batches"))
		}
		response, err := ingestMetricBatch(ctx, rpc.MetaFromContext(ctx), stream.Msg().Batch)
		if err != nil {
			return nil, err
		}
		if response.AcceptedSequence <= acceptedSequence {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("metrics sequences must increase within an upload stream"))
		}
		acceptedSequence = response.AcceptedSequence
		acceptedPoints += uint64(response.AcceptedPoints)
		if acceptedPoints > maxMetricUploadPoints {
			return nil, connectError(connect.CodeResourceExhausted, errors.New("metrics upload contains too many points"))
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if batches == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("metrics upload is empty"))
	}
	return connect.NewResponse(&metricsv1.UploadMetricsResponse{AcceptedSequence: acceptedSequence, AcceptedPoints: acceptedPoints}), nil
}

func (s *metricsService) StreamMetrics(ctx context.Context, stream *connect.BidiStream[metricsv1.StreamMetricsRequest, metricsv1.StreamMetricsResponse]) error {
	var acceptedSequence uint64
	for {
		request, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		response, err := ingestMetricBatch(ctx, rpc.MetaFromContext(ctx), request.Batch)
		if err != nil {
			return err
		}
		if response.AcceptedSequence <= acceptedSequence {
			return connectError(connect.CodeInvalidArgument, errors.New("metrics sequences must increase within a live stream"))
		}
		acceptedSequence = response.AcceptedSequence
		if err := stream.Send(&metricsv1.StreamMetricsResponse{
			AcceptedSequence: response.AcceptedSequence,
			AcceptedPoints:   response.AcceptedPoints,
		}); err != nil {
			return err
		}
	}
}

func (s *metricsService) WatchMetrics(ctx context.Context, req *connect.Request[metricsv1.WatchMetricsRequest], stream *connect.ServerStream[metricsv1.WatchMetricsResponse]) error {
	if len(req.Msg.AgentIds) > maxMetricWatchAgents || len(req.Msg.Metrics) > maxMetricWatchNames {
		return connectError(connect.CodeResourceExhausted, errors.New("metrics subscription is too large"))
	}
	period := defaultMetricWatchPeriod
	if req.Msg.MinimumInterval != nil {
		if err := req.Msg.MinimumInterval.CheckValid(); err != nil {
			return connectError(connect.CodeInvalidArgument, err)
		}
		period = req.Msg.MinimumInterval.AsDuration()
		if period < minimumMetricWatchPeriod || period > maximumMetricWatchPeriod {
			return connectError(connect.CodeInvalidArgument, fmt.Errorf("minimum_interval must be between %s and %s", minimumMetricWatchPeriod, maximumMetricWatchPeriod))
		}
	}
	agentIDs, err := connectVisibleAgentIDs(ctx, req.Msg.AgentIds)
	if err != nil {
		return err
	}
	requested := stringSet(req.Msg.Metrics)
	last := make(map[string]time.Time, len(agentIDs))
	for {
		latest := agentRuntimeReports()
		for _, agentID := range agentIDs {
			report := latest[agentID]
			if report == nil || !report.UpdatedAt.After(last[agentID]) {
				continue
			}
			for _, point := range legacyReportMetricPoints(report) {
				if len(requested) > 0 && !requested[point.Metric] {
					continue
				}
				if err := stream.Send(&metricsv1.WatchMetricsResponse{AgentId: agentID, Point: point}); err != nil {
					return err
				}
			}
			last[agentID] = report.UpdatedAt
		}
		timer := time.NewTimer(period)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func agentRuntimeReports() map[string]*legacyv1.Report {
	return agentRuntime.GetLatestReport()
}

func legacyReportMetricPoints(report *legacyv1.Report) []*metricsv1.MetricsPoint {
	observedAt := timestamppb.New(report.UpdatedAt)
	point := func(name string, value float64, labels map[string]string) *metricsv1.MetricsPoint {
		return &metricsv1.MetricsPoint{Metric: name, Value: value, ObservedAt: observedAt, Labels: labels}
	}
	points := []*metricsv1.MetricsPoint{
		point(metricstore.MetricCPU, report.CPU.Usage, nil),
		point(metricstore.MetricRAM, float64(report.Ram.Used), nil),
		point(metricstore.MetricSwap, float64(report.Swap.Used), nil),
		point(metricstore.MetricLoad, report.Load.Load1, nil),
		point(metricstore.MetricDisk, float64(report.Disk.Used), nil),
		point(metricstore.MetricNetIn, float64(report.Network.Down), nil),
		point(metricstore.MetricNetOut, float64(report.Network.Up), nil),
		point(metricstore.MetricNetTotalUp, float64(report.Network.TotalUp), nil),
		point(metricstore.MetricNetTotalDown, float64(report.Network.TotalDown), nil),
		point(metricstore.MetricProcess, float64(report.Process), nil),
		point(metricstore.MetricConnections, float64(report.Connections.TCP), nil),
		point(metricstore.MetricConnectionsUDP, float64(report.Connections.UDP), nil),
	}
	if report.GPU != nil {
		points = append(points, point(metricstore.MetricGPU, report.GPU.AverageUsage, nil))
		for index, gpu := range report.GPU.DetailedInfo {
			labels := map[string]string{"device": strconv.Itoa(index), "name": gpu.Name}
			points = append(points,
				point(metricstore.MetricGPUDeviceUsage, gpu.Utilization, labels),
				point(metricstore.MetricGPUMem, float64(gpu.MemoryUsed), labels),
				point(metricstore.MetricGPUMemTotal, float64(gpu.MemoryTotal), labels),
				point(metricstore.MetricGPUTemp, float64(gpu.Temperature), labels),
			)
		}
	}
	return points
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
