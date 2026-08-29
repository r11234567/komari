package connectapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/komari-monitor/komari/pkg/rpc"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	connectDefaultMetricPoints = 500
	connectMaxMetricPoints     = 10_000
	connectMaxRawMetricRange   = time.Hour
	connectMetricMemoryTotal   = "memory.total"
	connectMetricSwapTotal     = "swap.total"
	connectMetricDiskTotal     = "disk.total"
	// Queueing is deliberately short: the request deadline is reserved for
	// database execution rather than being consumed by an overloaded queue.
	connectMetricReadQueueWait = 2 * time.Second
	connectMetricLightCapacity = int64(8)
	connectMetricHeavyCapacity = int64(2)
)

// Recent reads have their own lane so multi-day scans cannot starve normal
// dashboard/live views.
var connectMetricLightReadGate = semaphore.NewWeighted(connectMetricLightCapacity)
var connectMetricHeavyReadGate = semaphore.NewWeighted(connectMetricHeavyCapacity)
var connectMetricQueryFlight singleflight.Group
var connectMetricQueryCache struct {
	sync.Mutex
	items map[string]metricQueryCacheEntry
}

type metricQueryCacheEntry struct {
	expiresAt time.Time
	value     *connect.Response[metricsv1.QueryMetricsResponse]
}

// acquireMetricReadSlot schedules public history scans instead of letting a
// browser burst run them all at once. It waits long enough for ordinary chart
// loads, then sheds only a sustained overload. Report ingestion, probe leases,
// and long-lived agent streams use separate paths and are never queued here.
func acquireMetricReadSlot(ctx context.Context, weight int64) (func(), error) {
	if weight < 1 {
		weight = 1
	}
	gate := connectMetricLightReadGate
	capacity := connectMetricLightCapacity
	if weight > 1 {
		gate = connectMetricHeavyReadGate
		capacity = connectMetricHeavyCapacity
	}
	if weight > capacity {
		weight = capacity
	}
	queued, cancel := context.WithTimeout(ctx, connectMetricReadQueueWait)
	defer cancel()
	if err := gate.Acquire(queued, weight); err != nil {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("metric query capacity is busy; retry shortly"))
	}
	return func() { gate.Release(weight) }, nil
}

func connectMetricReadWeight(start, end time.Time) int64 {
	duration := end.Sub(start)
	if duration <= time.Hour {
		return 1
	}
	if duration <= 24*time.Hour {
		return 2
	}
	return 3
}

var connectDerivedMetricDefinitions = []metric.Definition{
	{Name: connectMetricMemoryTotal, Description: "Installed memory", Type: metric.TypeGauge, Unit: "bytes"},
	{Name: connectMetricSwapTotal, Description: "Configured swap", Type: metric.TypeGauge, Unit: "bytes"},
	{Name: connectMetricDiskTotal, Description: "Configured disk capacity", Type: metric.TypeGauge, Unit: "bytes"},
}

// QueryMetrics is the canonical public historical-read path. It intentionally
// reads the metric store directly rather than calling the JSON-RPC adapter.
func (s *metricsService) QueryMetrics(ctx context.Context, req *connect.Request[metricsv1.QueryMetricsRequest]) (*connect.Response[metricsv1.QueryMetricsResponse], error) {
	// Dashboard refreshes from several browser tabs commonly ask for the exact
	// same window. Share an in-flight read so the request fan-out cannot multiply
	// SQLite work; the key includes the caller scope because visibility differs.
	key := connectMetricQueryKey(ctx, req.Msg)
	connectMetricQueryCache.Lock()
	if cached := connectMetricQueryCache.items[key]; cached.value != nil && time.Now().Before(cached.expiresAt) {
		connectMetricQueryCache.Unlock()
		return cached.value, nil
	}
	connectMetricQueryCache.Unlock()
	value, err, _ := connectMetricQueryFlight.Do(key, func() (any, error) {
		result, queryErr := s.queryMetrics(ctx, req)
		if queryErr == nil {
			connectMetricQueryCache.Lock()
			if connectMetricQueryCache.items == nil {
				connectMetricQueryCache.items = make(map[string]metricQueryCacheEntry)
			}
			if len(connectMetricQueryCache.items) >= 512 {
				for cachedKey, cached := range connectMetricQueryCache.items {
					if time.Now().After(cached.expiresAt) {
						delete(connectMetricQueryCache.items, cachedKey)
					}
				}
				if len(connectMetricQueryCache.items) >= 512 {
					connectMetricQueryCache.items = make(map[string]metricQueryCacheEntry)
				}
			}
			connectMetricQueryCache.items[key] = metricQueryCacheEntry{expiresAt: time.Now().Add(750 * time.Millisecond), value: result}
			connectMetricQueryCache.Unlock()
		}
		return result, queryErr
	})
	if err != nil {
		return nil, err
	}
	return value.(*connect.Response[metricsv1.QueryMetricsResponse]), nil
}

func connectMetricQueryKey(ctx context.Context, request *metricsv1.QueryMetricsRequest) string {
	// Browsers compute "now" independently, so equivalent dashboard windows
	// otherwise miss singleflight by a few milliseconds. Canonicalize only the
	// cache key; the first request still owns the exact response boundaries.
	canonical := proto.Clone(request).(*metricsv1.QueryMetricsRequest)
	const quantum = 5 * time.Second
	if canonical.StartTime != nil && canonical.StartTime.IsValid() {
		canonical.StartTime = timestamppb.New(canonical.StartTime.AsTime().UTC().Truncate(quantum))
	}
	if canonical.EndTime != nil && canonical.EndTime.IsValid() {
		canonical.EndTime = timestamppb.New(canonical.EndTime.AsTime().UTC().Truncate(quantum))
	}
	// These request spellings have identical server semantics, but protobuf
	// encoding would otherwise give them different singleflight keys.
	if canonical.Downsample == nil {
		defaultDownsample := true
		canonical.Downsample = &defaultDownsample
	}
	if strings.TrimSpace(canonical.Aggregation) == "" {
		canonical.Aggregation = string(metric.AggAvg)
	}
	if canonical.MaxPoints == 0 {
		canonical.MaxPoints = connectDefaultMetricPoints
	}
	sort.Strings(canonical.AgentIds)
	sort.Strings(canonical.Metrics)
	encoded, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(canonical)
	meta := rpc.MetaFromContext(ctx)
	scope := "guest"
	if meta != nil && meta.Principal != nil {
		scope = strconv.Itoa(int(meta.Principal.Type)) + ":" + meta.Principal.UserUUID + ":" + meta.Principal.ClientUUID
	}
	digest := sha256.Sum256(append([]byte(scope+"\x00"), encoded...))
	return hex.EncodeToString(digest[:])
}

func (s *metricsService) queryMetrics(ctx context.Context, req *connect.Request[metricsv1.QueryMetricsRequest]) (*connect.Response[metricsv1.QueryMetricsResponse], error) {
	if len(req.Msg.Metrics) == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("at least one metric is required"))
	}
	start, end, err := connectMetricWindow(req.Msg.StartTime, req.Msg.EndTime)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	if req.Msg.Downsample != nil && !*req.Msg.Downsample && end.Sub(start) > connectMaxRawMetricRange {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("raw metric queries are limited to one hour"))
	}
	maxPoints := int(req.Msg.MaxPoints)
	if maxPoints == 0 {
		maxPoints = connectDefaultMetricPoints
	}
	if maxPoints < 1 || maxPoints > connectMaxMetricPoints {
		return nil, connectError(connect.CodeInvalidArgument, fmtRange("max_points must be between 1 and %d", connectMaxMetricPoints))
	}
	entityIDs, err := connectVisibleAgentIDs(ctx, req.Msg.AgentIds)
	if err != nil {
		return nil, err
	}
	store := metricstore.GetStore()
	if store == nil {
		return nil, connectError(connect.CodeFailedPrecondition, errors.New("metric store is not initialized"))
	}
	release, err := acquireMetricReadSlot(ctx, connectMetricReadWeight(start, end))
	if err != nil {
		return nil, err
	}
	defer release()
	definitions, err := store.ListMetrics(ctx)
	if err != nil {
		return nil, connectMetricStoreError(err)
	}
	definitionByName := make(map[string]metric.Definition, len(definitions))
	for _, definition := range definitions {
		definitionByName[definition.Name] = definition
	}
	for _, definition := range connectDerivedMetricDefinitions {
		definitionByName[definition.Name] = definition
	}

	downsample := req.Msg.Downsample == nil || *req.Msg.Downsample
	aggregation := metric.Aggregation(strings.ToLower(strings.TrimSpace(req.Msg.Aggregation)))
	if aggregation == "" {
		aggregation = metric.AggAvg
	}
	interval := metric.CeilStandardInterval(time.Duration((end.Sub(start).Nanoseconds() + int64(maxPoints) - 1) / int64(maxPoints)))
	if interval < time.Second {
		interval = time.Second
	}
	interval = store.CompatibleSeriesInterval(start, time.Now().UTC(), interval)

	resultParts := make([][]*metricsv1.MetricsSeries, 0, len(req.Msg.Metrics)*len(entityIDs))
	derivedTotals, err := connectDerivedMetricValues(entityIDs)
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	queries := make([]metric.AggregateQuery, 0)
	rawQueries := make([]metric.Query, 0)
	type pending struct {
		index      int
		metricName string
		agentID    string
		definition metric.Definition
	}
	pendingDownsample := make([]pending, 0)
	pendingRaw := make([]pending, 0)
	for _, metricName := range uniqueStrings(req.Msg.Metrics) {
		definition, ok := definitionByName[metricName]
		if !ok {
			return nil, connectError(connect.CodeInvalidArgument, fmtRange("unknown metric %q", metricName))
		}
		for _, agentID := range entityIDs {
			if value, ok := derivedTotals[metricName][agentID]; ok {
				resultParts = append(resultParts, []*metricsv1.MetricsSeries{connectDerivedMetricSeries(metricName, agentID, definition, value, start, end, downsample, aggregation, interval)})
				continue
			}
			query := metric.Query{MetricName: metricName, EntityID: agentID, Start: start, End: end, Tags: req.Msg.Tags, Order: metric.OrderAsc}
			if downsample {
				pendingDownsample = append(pendingDownsample, pending{index: len(resultParts), metricName: metricName, agentID: agentID, definition: definition})
				queries = append(queries, metric.AggregateQuery{Query: query, Aggregation: aggregation, Interval: interval, PreserveSeries: true})
				resultParts = append(resultParts, nil)
				continue
			}
			pendingRaw = append(pendingRaw, pending{index: len(resultParts), metricName: metricName, agentID: agentID, definition: definition})
			rawQueries = append(rawQueries, query)
			resultParts = append(resultParts, nil)
		}
	}
	if downsample && len(queries) > 0 {
		var batch [][]metric.AggregatePoint
		var queryErr error
		// SQLite V4 can route all ordinary dashboard aggregations through one
		// snapshot. This avoids one block/axis scan per metric in a six-metric
		// page while retaining SeriesBatch for percentile/rate compatibility.
		if aggregation == metric.AggAvg || aggregation == metric.AggSum || aggregation == metric.AggLast {
			batch, queryErr = store.DashboardSeriesBatch(ctx, queries, time.Now().UTC())
		} else {
			batch, queryErr = store.SeriesBatch(ctx, queries, time.Now().UTC())
		}
		if queryErr != nil {
			return nil, connectMetricStoreError(queryErr)
		}
		for index, item := range pendingDownsample {
			if index >= len(batch) || index >= len(queries) {
				break
			}
			if len(batch[index]) > 0 {
				resultParts[item.index] = connectAggregateSeries(item.metricName, item.agentID, item.definition, aggregation, interval, req.Msg.FillEmpty, start, end, batch[index])
			}
		}
	}
	if !downsample && len(rawQueries) > 0 {
		batch, queryErr := store.QueryRawBatch(ctx, rawQueries)
		if queryErr != nil {
			return nil, connectMetricStoreError(queryErr)
		}
		for index, item := range pendingRaw {
			if index >= len(batch) || index >= len(rawQueries) {
				break
			}
			resultParts[item.index] = connectRawSeries(item.metricName, item.agentID, item.definition, batch[index])
		}
	}
	result := make([]*metricsv1.MetricsSeries, 0, len(resultParts))
	for _, part := range resultParts {
		result = append(result, part...)
	}
	return connect.NewResponse(&metricsv1.QueryMetricsResponse{Series: result}), nil
}

func (s *metricsService) ListMetricDefinitions(ctx context.Context, _ *connect.Request[metricsv1.ListMetricDefinitionsRequest]) (*connect.Response[metricsv1.ListMetricDefinitionsResponse], error) {
	store := metricstore.GetStore()
	if store == nil {
		return nil, connectError(connect.CodeFailedPrecondition, errors.New("metric store is not initialized"))
	}
	definitions, err := store.ListMetrics(ctx)
	if err != nil {
		return nil, connectMetricStoreError(err)
	}
	result := make([]*metricsv1.MetricDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, &metricsv1.MetricDefinition{Name: definition.Name, Description: definition.Description, Type: string(definition.Type), Unit: definition.Unit, RetentionDays: uint32(max(definition.RetentionDays, 0)), Metadata: definition.Metadata, CreatedAt: timestamppb.New(definition.CreatedAt), UpdatedAt: timestamppb.New(definition.UpdatedAt)})
	}
	for _, definition := range connectDerivedMetricDefinitions {
		result = append(result, &metricsv1.MetricDefinition{Name: definition.Name, Description: definition.Description, Type: string(definition.Type), Unit: definition.Unit})
	}
	return connect.NewResponse(&metricsv1.ListMetricDefinitionsResponse{Definitions: result}), nil
}

func (s *metricsService) ListPingTasks(ctx context.Context, _ *connect.Request[metricsv1.ListPingTasksRequest]) (*connect.Response[metricsv1.ListPingTasksResponse], error) {
	taskList, err := tasks.GetAllPingTasks()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	result := make([]*metricsv1.PingTask, 0, len(taskList))
	for _, task := range taskList {
		result = append(result, &metricsv1.PingTask{TaskId: uint64(task.Id), Name: task.Name, Type: task.Type, Interval: durationpb.New(time.Duration(max(task.Interval, 0)) * time.Second)})
	}
	return connect.NewResponse(&metricsv1.ListPingTasksResponse{Tasks: result}), nil
}

func (s *metricsService) GetPingStats(ctx context.Context, req *connect.Request[metricsv1.GetPingStatsRequest]) (*connect.Response[metricsv1.GetPingStatsResponse], error) {
	start, end, err := connectMetricWindow(req.Msg.StartTime, req.Msg.EndTime)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	maxPoints := int(req.Msg.MaxPoints)
	if maxPoints == 0 {
		maxPoints = connectDefaultMetricPoints
	}
	if maxPoints < 1 || maxPoints > connectMaxMetricPoints {
		return nil, connectError(connect.CodeInvalidArgument, fmtRange("max_points must be between 1 and %d", connectMaxMetricPoints))
	}
	release, err := acquireMetricReadSlot(ctx, connectMetricReadWeight(start, end))
	if err != nil {
		return nil, err
	}
	defer release()
	entityIDs, err := connectVisibleAgentIDs(ctx, req.Msg.AgentIds)
	if err != nil {
		return nil, err
	}
	store := metricstore.GetStore()
	if store == nil {
		return nil, connectError(connect.CodeFailedPrecondition, errors.New("metric store is not initialized"))
	}
	taskList, err := tasks.GetAllPingTasks()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	taskByID := make(map[string]models.PingTask, len(taskList))
	for _, task := range taskList {
		taskByID[strconv.FormatUint(uint64(task.Id), 10)] = task
	}
	taskFilter := make(map[string]bool, len(req.Msg.TaskIds))
	for _, id := range req.Msg.TaskIds {
		taskFilter[strconv.FormatUint(id, 10)] = true
	}
	interval := metric.CeilStandardInterval(time.Duration((end.Sub(start).Nanoseconds() + int64(maxPoints) - 1) / int64(maxPoints)))
	interval = store.CompatibleSeriesInterval(start, time.Now().UTC(), interval)
	result := make([]*metricsv1.PingStat, 0)
	for _, agentID := range entityIDs {
		summary, queryErr := store.PingSeriesSummary(ctx, metric.AggregateQuery{Query: metric.Query{MetricName: metricstore.MetricPingLatency, EntityID: agentID, Start: start, End: end, Order: metric.OrderAsc}, Interval: interval, PreserveSeries: true}, time.Now().UTC())
		if queryErr != nil {
			return nil, connectMetricStoreError(queryErr)
		}
		result = append(result, connectPingStats(agentID, summary, taskByID, taskFilter)...)
	}
	return connect.NewResponse(&metricsv1.GetPingStatsResponse{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end), Interval: durationpb.New(interval), Stats: result}), nil
}

func connectMetricWindow(startValue, endValue *timestamppb.Timestamp) (time.Time, time.Time, error) {
	end := time.Now().UTC()
	if endValue != nil {
		if !endValue.IsValid() {
			return time.Time{}, time.Time{}, errors.New("end_time is invalid")
		}
		end = endValue.AsTime().UTC()
	}
	start := end.Add(-4 * time.Hour)
	if startValue != nil {
		if !startValue.IsValid() {
			return time.Time{}, time.Time{}, errors.New("start_time is invalid")
		}
		start = startValue.AsTime().UTC()
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("end_time must be after start_time")
	}
	return start, end, nil
}

func connectDerivedMetricValues(agentIDs []string) (map[string]map[string]float64, error) {
	clientList, err := clients.GetClientBasicInfoByUUIDs(agentIDs)
	if err != nil {
		return nil, err
	}
	values := map[string]map[string]float64{
		connectMetricMemoryTotal: {},
		connectMetricSwapTotal:   {},
		connectMetricDiskTotal:   {},
	}
	for _, client := range clientList {
		values[connectMetricMemoryTotal][client.UUID] = float64(max(client.MemTotal, 0))
		values[connectMetricSwapTotal][client.UUID] = float64(max(client.SwapTotal, 0))
		values[connectMetricDiskTotal][client.UUID] = float64(max(client.DiskTotal, 0))
	}
	return values, nil
}

func connectDerivedMetricSeries(metricName, agentID string, definition metric.Definition, value float64, start, end time.Time, downsample bool, aggregation metric.Aggregation, interval time.Duration) *metricsv1.MetricsSeries {
	series := &metricsv1.MetricsSeries{
		AgentId: agentID, Metric: metricName, Type: string(definition.Type), Unit: definition.Unit,
		Downsampled: downsample,
	}
	if downsample {
		series.Aggregation = string(aggregation)
		series.Interval = durationpb.New(interval)
	}
	series.QueryPoints = []*metricsv1.QueryPoint{
		{ObservedAt: timestamppb.New(start), Value: proto.Float64(value)},
		{ObservedAt: timestamppb.New(end), Value: proto.Float64(value)},
	}
	return series
}

func connectVisibleAgentIDs(ctx context.Context, requested []string) ([]string, error) {
	all, err := clients.GetAllClientBasicInfo()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	meta := rpc.MetaFromContext(ctx)
	isAdmin := meta != nil && meta.Principal != nil && meta.Principal.HasRole(rpc.RoleAdmin)
	wanted := stringSet(requested)
	result := make([]string, 0, len(all))
	for _, client := range all {
		if client.Hidden && !isAdmin || len(wanted) > 0 && !wanted[client.UUID] {
			continue
		}
		result = append(result, client.UUID)
	}
	return result, nil
}

func connectAggregateSeries(metricName, agentID string, definition metric.Definition, aggregation metric.Aggregation, interval time.Duration, fillEmpty bool, start, end time.Time, points []metric.AggregatePoint) []*metricsv1.MetricsSeries {
	groups := make(map[string]*metricsv1.MetricsSeries)
	for _, point := range points {
		key := metricTagsKey(point.Tags)
		series := groups[key]
		if series == nil {
			series = &metricsv1.MetricsSeries{AgentId: agentID, Metric: metricName, Labels: cloneLabels(point.Tags), Type: string(definition.Type), Unit: definition.Unit, RetentionDays: uint32(max(definition.RetentionDays, 0)), Downsampled: true, Aggregation: string(aggregation), Interval: durationpb.New(interval)}
			groups[key] = series
		}
		value := point.Value
		if fillEmpty && (metricName == metricstore.MetricPingLatency || metricName == metricstore.MetricPingLoss) && value == -1 {
			series.QueryPoints = append(series.QueryPoints, &metricsv1.QueryPoint{ObservedAt: timestamppb.New(point.Bucket), SampleCount: uint32(max(point.Count, 0)), Labels: cloneLabels(point.Tags)})
		} else {
			series.QueryPoints = append(series.QueryPoints, &metricsv1.QueryPoint{ObservedAt: timestamppb.New(point.Bucket), Value: proto.Float64(value), SampleCount: uint32(max(point.Count, 0)), Labels: cloneLabels(point.Tags)})
		}
	}
	result := make([]*metricsv1.MetricsSeries, 0, len(groups))
	for _, series := range groups {
		if fillEmpty && len(series.QueryPoints) == 0 {
			series.QueryPoints = []*metricsv1.QueryPoint{{ObservedAt: timestamppb.New(start), Labels: cloneLabels(series.Labels)}, {ObservedAt: timestamppb.New(end), Labels: cloneLabels(series.Labels)}}
		}
		result = append(result, series)
	}
	return result
}

func connectRawSeries(metricName, agentID string, definition metric.Definition, points []metric.Point) []*metricsv1.MetricsSeries {
	groups := make(map[string]*metricsv1.MetricsSeries)
	for _, point := range points {
		key := metricTagsKey(point.Tags)
		series := groups[key]
		if series == nil {
			series = &metricsv1.MetricsSeries{AgentId: agentID, Metric: metricName, Labels: cloneLabels(point.Tags), Type: string(definition.Type), Unit: definition.Unit, RetentionDays: uint32(max(definition.RetentionDays, 0))}
			groups[key] = series
		}
		series.QueryPoints = append(series.QueryPoints, &metricsv1.QueryPoint{ObservedAt: timestamppb.New(point.Timestamp), Value: proto.Float64(point.Value), Labels: cloneLabels(point.Labels)})
	}
	result := make([]*metricsv1.MetricsSeries, 0, len(groups))
	for _, series := range groups {
		result = append(result, series)
	}
	return result
}

func connectPingStats(agentID string, summary metric.SeriesSummary, tasksByID map[string]models.PingTask, filter map[string]bool) []*metricsv1.PingStat {
	group := func(points []metric.AggregatePoint) map[string][]metric.AggregatePoint {
		out := make(map[string][]metric.AggregatePoint)
		for _, point := range points {
			if id := strings.TrimSpace(point.Tags["task_id"]); id != "" {
				out[id] = append(out[id], point)
			}
		}
		return out
	}
	avg, minimum, maximum, latest, p50, p99, stddev, loss := group(summary.Avg), group(summary.Min), group(summary.Max), group(summary.Last), group(summary.P50), group(summary.P99), group(summary.StdDev), group(summary.Loss)
	ids := make(map[string]bool)
	for _, values := range []map[string][]metric.AggregatePoint{avg, minimum, maximum, latest, p50, p99, stddev, loss} {
		for id := range values {
			ids[id] = true
		}
	}
	result := make([]*metricsv1.PingStat, 0, len(ids))
	for id := range ids {
		if len(filter) > 0 && !filter[id] {
			continue
		}
		total := aggregateCount(avg[id])
		if total == 0 {
			total = aggregateCount(loss[id])
		}
		if total == 0 {
			continue
		}
		lossPercent, valid, approximate := connectLossRate(avg[id], loss[id], total)
		stat := &metricsv1.PingStat{AgentId: agentID, TaskId: parseUint(id), Tags: map[string]string{"task_id": id}, Total: uint32(total), Valid: uint32(valid), LossPercent: lossPercent, LossApproximate: approximate, Minimum: aggregateMin(minimum[id]), Maximum: aggregateMax(maximum[id]), Average: aggregateWeighted(avg[id], true), Latest: aggregateLatest(latest[id]), P50: aggregateWeighted(p50[id], true), P99: aggregateWeighted(p99[id], true), StandardDeviation: aggregateWeighted(stddev[id], false)}
		if stat.Latest == nil {
			stat.Latest = aggregateLatest(avg[id])
		}
		if stat.P50 != nil && stat.P99 != nil && *stat.P50 > 0 && *stat.P99 >= *stat.P50 {
			stat.P99P50Ratio = (*stat.P99 - *stat.P50) / math.Max(math.Min(*stat.P50, 50), 10)
		}
		if task, ok := tasksByID[id]; ok {
			stat.Name, stat.Type, stat.ProbeInterval = task.Name, task.Type, durationpb.New(time.Duration(max(task.Interval, 0))*time.Second)
		}
		result = append(result, stat)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TaskId < result[j].TaskId })
	return result
}

func connectMetricStoreError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return connectError(connect.CodeCanceled, err)
	}
	return connectError(connect.CodeInternal, err)
}

func fmtRange(format string, args ...any) error { return fmt.Errorf(format, args...) }
func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
func cloneLabels(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
func metricTagsKey(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		out.WriteString(key)
		out.WriteByte('=')
		out.WriteString(values[key])
		out.WriteByte(0)
	}
	return out.String()
}
func aggregateCount(points []metric.AggregatePoint) int {
	total := 0
	for _, point := range points {
		total += max(point.Count, 0)
	}
	return total
}
func aggregateWeighted(points []metric.AggregatePoint, positive bool) *float64 {
	total, count := 0.0, 0
	for _, point := range points {
		if point.Count <= 0 || positive && point.Value < 0 {
			continue
		}
		total += point.Value * float64(point.Count)
		count += point.Count
	}
	if count == 0 {
		return nil
	}
	return proto.Float64(total / float64(count))
}
func aggregateMin(points []metric.AggregatePoint) *float64 {
	var out *float64
	for _, point := range points {
		if point.Count > 0 && point.Value >= 0 && (out == nil || point.Value < *out) {
			out = proto.Float64(point.Value)
		}
	}
	return out
}
func aggregateMax(points []metric.AggregatePoint) *float64 {
	var out *float64
	for _, point := range points {
		if point.Count > 0 && point.Value >= 0 && (out == nil || point.Value > *out) {
			out = proto.Float64(point.Value)
		}
	}
	return out
}
func aggregateLatest(points []metric.AggregatePoint) *float64 {
	var out *float64
	var at time.Time
	for _, point := range points {
		if point.Count > 0 && point.Value >= 0 && (out == nil || point.Bucket.After(at)) {
			out, at = proto.Float64(point.Value), point.Bucket
		}
	}
	return out
}
func connectLossRate(latency, loss []metric.AggregatePoint, total int) (float64, int, bool) {
	lost := 0.0
	available := false
	for _, point := range loss {
		if point.Count > 0 {
			available = true
			lost += math.Max(0, math.Min(1, point.Value)) * float64(point.Count)
		}
	}
	if available {
		lost = math.Min(lost, float64(total))
		return lost / float64(total) * 100, total - int(math.Round(lost)), false
	}
	failures := 0
	for _, point := range latency {
		if point.Count > 0 && point.Value < 0 {
			failures += point.Count
		}
	}
	return float64(failures) / float64(total) * 100, total - failures, true
}
func parseUint(value string) uint64 { parsed, _ := strconv.ParseUint(value, 10, 64); return parsed }
