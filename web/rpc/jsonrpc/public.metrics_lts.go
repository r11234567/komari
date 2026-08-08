package jsonrpc

import (
	"context"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/history"
	"github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
)

type ltsMetricDefinition struct {
	Name          string  `json:"name"`
	Description   string  `json:"description"`
	Type          string  `json:"type"`
	Unit          string  `json:"unit,omitempty"`
	RetentionDays float64 `json:"retention_days"`
}

type ltsMetricPoint struct {
	Time  time.Time `json:"time"`
	Value *float64  `json:"value"`
	Count int       `json:"count,omitempty"`
}

type ltsMetricSeries struct {
	MetricKey           string            `json:"metric_key"`
	EntityID            string            `json:"entity_id"`
	Type                string            `json:"type"`
	Unit                string            `json:"unit,omitempty"`
	RetentionDays       float64           `json:"retention_days"`
	Downsampled         bool              `json:"downsampled"`
	DownsampleAlgorithm string            `json:"downsample_algorithm"`
	MaxPoints           int               `json:"max_points"`
	IntervalSeconds     int64             `json:"interval_seconds"`
	Tags                map[string]string `json:"tags,omitempty"`
	Count               int               `json:"count"`
	Points              []ltsMetricPoint  `json:"points"`
}

type ltsMetricQueryParams struct {
	MetricKeys  []string `json:"metric_keys"`
	EntityID    string   `json:"entity_id"`
	EntityIDs   []string `json:"entity_ids"`
	UUID        string   `json:"uuid"`
	Hours       float64  `json:"hours"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	MaxPoints   int      `json:"max_points"`
	Aggregation string   `json:"aggregation"`
}

var ltsMetricDefinitions = []struct {
	key         string
	description string
	unit        string
	source      string
}{
	{"cpu.usage", "CPU", "%", "load"},
	{"gpu.usage", "GPU", "%", "load"},
	{"gpu.device.usage", "GPU Device", "%", "gpu"},
	{"gpu.memory.used", "GPU Memory", "bytes", "gpu"},
	{"gpu.memory.total", "GPU Memory Total", "bytes", "gpu"},
	{"gpu.temperature", "GPU Temperature", "degC", "gpu"},
	{"memory.used", "RAM", "bytes", "load"},
	{"memory.total", "RAM Total", "bytes", "load"},
	{"swap.used", "Swap", "bytes", "load"},
	{"swap.total", "Swap Total", "bytes", "load"},
	{"load.average", "Load", "", "load"},
	{"temperature", "Temperature", "degC", "load"},
	{"disk.used", "Disk", "bytes", "load"},
	{"disk.total", "Disk Total", "bytes", "load"},
	{"net.in.rate", "Download", "bytes/s", "load"},
	{"net.out.rate", "Upload", "bytes/s", "load"},
	{"net.total.up", "Total Upload", "bytes", "load"},
	{"net.total.down", "Total Download", "bytes", "load"},
	{"traffic.up", "Traffic Upload", "bytes", "load"},
	{"traffic.down", "Traffic Download", "bytes", "load"},
	{"process.count", "Processes", "count", "load"},
	{"connections.tcp", "TCP Connections", "count", "load"},
	{"connections.udp", "UDP Connections", "count", "load"},
	{"ping.latency_ms", "Ping", "ms", "ping"},
	{"ping.loss", "Ping Loss", "", "ping"},
}

const ltsMetricRetentionConfigKey = "metric_retention_days_by_name"

var ltsMetricRetentionMu sync.Mutex

func init() {
	regPublic("listMetricDefinitions", publicListMetricDefinitions, "List 1.2.5 LTS metric definitions")
	regPublic("queryMetrics", publicQueryMetrics, "Query bounded 1.2.5 LTS metrics")
	regPublic("getPingMetricStats", publicGetPingMetricStats, "Get bounded Ping statistics")
	RegisterWithGroupAndMeta("listMetricDefinitions", rpc.RoleAdmin, publicListMetricDefinitions, &rpc.MethodMeta{
		Name: "admin:listMetricDefinitions", Summary: "List 1.2.5 LTS metric definitions",
	})
}

func ltsDefaultMetricRetentionDays() map[string]int {
	settings, err := config.GetManyAs[config.Settings]()
	loadDays, pingDays := 30, 1
	if err == nil && settings != nil {
		loadDays = int(math.Ceil(float64(settings.RecordPreserveTime) / 24))
		pingDays = int(math.Ceil(float64(settings.PingRecordPreserveTime) / 24))
	}
	defaults := make(map[string]int, len(ltsMetricDefinitions))
	for _, item := range ltsMetricDefinitions {
		if item.source == "ping" {
			defaults[item.key] = pingDays
		} else {
			defaults[item.key] = loadDays
		}
	}
	return defaults
}

func ltsMetricRetentionDays() map[string]int {
	ltsMetricRetentionMu.Lock()
	defer ltsMetricRetentionMu.Unlock()
	return ltsMetricRetentionDaysLocked()
}

func ltsMetricRetentionDaysLocked() map[string]int {
	defaults := ltsDefaultMetricRetentionDays()
	stored, err := config.GetAs[map[string]int](ltsMetricRetentionConfigKey)
	if err != nil || stored == nil {
		_ = config.Set(ltsMetricRetentionConfigKey, defaults)
		return defaults
	}
	changed := false
	for name, days := range defaults {
		if _, ok := stored[name]; !ok {
			stored[name] = days
			changed = true
		}
	}
	if changed {
		_ = config.Set(ltsMetricRetentionConfigKey, stored)
	}
	return stored
}

func ltsMetricDefinitionByName(name string) (source string, ok bool) {
	for _, item := range ltsMetricDefinitions {
		if item.key == name {
			return item.source, true
		}
	}
	return "", false
}

func publicListMetricDefinitions(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	retention := ltsMetricRetentionDays()
	definitions := make([]ltsMetricDefinition, 0, len(ltsMetricDefinitions))
	for _, item := range ltsMetricDefinitions {
		definitions = append(definitions, ltsMetricDefinition{
			Name: item.key, Description: item.description, Type: "gauge",
			Unit: item.unit, RetentionDays: float64(retention[item.key]),
		})
	}
	return definitions, nil
}

func publicQueryMetrics(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params ltsMetricQueryParams
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid metric query: "+err.Error(), nil)
	}
	if len(params.MetricKeys) == 0 {
		return nil, rpc.MakeError(rpc.InvalidParams, "metric_keys are required", nil)
	}
	entityIDs, entityErr := ltsMetricEntityIDs(ctx, params.EntityID, params.EntityIDs)
	if entityErr != nil {
		return nil, entityErr
	}

	queryRequest, err := ltsMetricHistoryRequest(params)
	if err != nil {
		return nil, historyRPCError(err, "Invalid metric query")
	}
	ctx, cancel := context.WithTimeout(ctx, history.QueryTimeout)
	defer cancel()

	requested := make(map[string]bool, len(params.MetricKeys))
	retention := ltsMetricRetentionDays()
	loadRetentionDays, pingRetentionDays := 0, 0
	for _, key := range params.MetricKeys {
		requested[key] = true
		switch ltsMetricSource(key) {
		case "ping":
			pingRetentionDays = max(pingRetentionDays, retention[key])
		case "load", "gpu":
			loadRetentionDays = max(loadRetentionDays, retention[key])
		}
	}

	maxPoints := ltsMetricMaxPoints(params.MaxPoints)
	perEntityMaxPoints := maxPoints
	if len(entityIDs) > 0 {
		perEntityMaxPoints = max(1, maxPoints/len(entityIDs))
	}
	queryRequest.MaxPoints = perEntityMaxPoints
	series := make([]ltsMetricSeries, 0)
	var responseStart, responseEnd time.Time
	for _, entityID := range entityIDs {
		if loadRetentionDays > 0 {
			loadRequest := queryRequest
			loadRequest.Type = "load"
			loadRequest.UUID = entityID
			if clamped, ok := clampLTSHistoryRequest(loadRequest, loadRetentionDays); ok {
				result, queryErr := history.Query(ctx, clamped)
				if queryErr != nil {
					return nil, historyRPCError(queryErr, "Failed to query resource metrics")
				}
				if responseStart.IsZero() || result.Start.Before(responseStart) {
					responseStart = result.Start
				}
				if responseEnd.IsZero() || result.End.After(responseEnd) {
					responseEnd = result.End
				}
				series = append(series, ltsLoadMetricSeries(result, requested, entityID, retention)...)
			}
		}
		if pingRetentionDays > 0 {
			pingRequest := queryRequest
			pingRequest.Type = "ping"
			pingRequest.UUID = entityID
			if clamped, ok := clampLTSHistoryRequest(pingRequest, pingRetentionDays); ok {
				result, queryErr := history.Query(ctx, clamped)
				if queryErr != nil {
					return nil, historyRPCError(queryErr, "Failed to query Ping metrics")
				}
				if responseStart.IsZero() || result.Start.Before(responseStart) {
					responseStart = result.Start
				}
				if responseEnd.IsZero() || result.End.After(responseEnd) {
					responseEnd = result.End
				}
				series = append(series, ltsPingMetricSeries(result, requested, entityID, retention)...)
			}
		}
	}
	if responseStart.IsZero() {
		responseStart, responseEnd, err = ltsMetricRange(params)
		if err != nil {
			return nil, historyRPCError(err, "Invalid metric query")
		}
	}

	series = limitLTSMetricPoints(series, maxPoints)
	total := 0
	for index := range series {
		series[index].Count = len(series[index].Points)
		series[index].MaxPoints = maxPoints
		total += series[index].Count
	}
	return map[string]any{
		"start": responseStart, "end": responseEnd, "series": series, "count": total,
	}, nil
}

func ltsMetricEntityIDs(ctx context.Context, requested string, requestedMany []string) ([]string, *rpc.JsonRpcError) {
	clientList, err := clients.GetAllClientBasicInfo()
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to retrieve client information: "+err.Error(), nil)
	}
	isLogin := isLoginFromCtx(ctx)
	visible := make(map[string]struct{}, len(clientList))
	for _, client := range clientList {
		if client.Hidden && !isLogin {
			continue
		}
		visible[client.UUID] = struct{}{}
	}

	requestedIDs := append([]string(nil), requestedMany...)
	if requested != "" {
		requestedIDs = append([]string{requested}, requestedIDs...)
	}
	if len(requestedIDs) == 0 {
		entityIDs := make([]string, 0, len(visible))
		for _, client := range clientList {
			if _, ok := visible[client.UUID]; ok {
				entityIDs = append(entityIDs, client.UUID)
			}
		}
		return entityIDs, nil
	}

	entityIDs := make([]string, 0, len(requestedIDs))
	seen := make(map[string]struct{}, len(requestedIDs))
	for _, entityID := range requestedIDs {
		if entityID == "" {
			continue
		}
		if _, ok := visible[entityID]; !ok {
			return nil, rpc.MakeError(rpc.NotFound, "client not found", nil)
		}
		if _, duplicate := seen[entityID]; duplicate {
			continue
		}
		seen[entityID] = struct{}{}
		entityIDs = append(entityIDs, entityID)
	}
	return entityIDs, nil
}

func clampLTSHistoryRequest(request history.QueryRequest, retentionDays int) (history.QueryRequest, bool) {
	if retentionDays <= 0 {
		return request, false
	}
	start, err := time.Parse(time.RFC3339, request.Start)
	if err != nil {
		return request, true
	}
	end, err := time.Parse(time.RFC3339, request.End)
	if err != nil {
		return request, true
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	if !end.After(cutoff) {
		return request, false
	}
	if start.Before(cutoff) {
		request.Start = cutoff.Format(time.RFC3339)
	}
	return request, true
}

func ltsMetricHistoryRequest(params ltsMetricQueryParams) (history.QueryRequest, error) {
	start, end, err := ltsMetricRange(params)
	if err != nil {
		return history.QueryRequest{}, err
	}
	return history.QueryRequest{
		Start: start.Format(time.RFC3339), End: end.Format(time.RFC3339),
		MaxPoints: ltsMetricMaxPoints(params.MaxPoints),
	}, nil
}

func ltsMetricRange(params ltsMetricQueryParams) (time.Time, time.Time, error) {
	end := time.Now()
	var err error
	if params.End != "" {
		end, err = time.Parse(time.RFC3339, params.End)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if params.Start != "" {
		start, parseErr := time.Parse(time.RFC3339, params.Start)
		if parseErr != nil {
			return time.Time{}, time.Time{}, parseErr
		}
		if !end.After(start) || end.Sub(start) > 90*24*time.Hour {
			return time.Time{}, time.Time{}, history.ErrRangeTooLarge
		}
		return start, end, nil
	}
	hours := params.Hours
	if hours <= 0 {
		hours = 1
	}
	duration := time.Duration(hours * float64(time.Hour))
	if duration <= 0 || duration > 90*24*time.Hour {
		return time.Time{}, time.Time{}, history.ErrRangeTooLarge
	}
	return end.Add(-duration), end, nil
}

func ltsMetricMaxPoints(value int) int {
	if value <= 0 {
		return history.DefaultMaxPoints
	}
	if value > history.MaxMaxPoints {
		return history.MaxMaxPoints
	}
	return value
}

func ltsMetricSource(key string) string {
	for _, item := range ltsMetricDefinitions {
		if item.key == key {
			return item.source
		}
	}
	return ""
}

func ltsLoadMetricSeries(result *history.Response, requested map[string]bool, entityID string, retention map[string]int) []ltsMetricSeries {
	interval := ltsResolutionSeconds(result.Resolution)
	series := make([]ltsMetricSeries, 0, len(requested))
	for _, source := range result.Series {
		switch source.Kind {
		case "load":
			for _, definition := range ltsMetricDefinitions {
				if definition.source != "load" || !requested[definition.key] {
					continue
				}
				days := retention[definition.key]
				points := make([]ltsMetricPoint, 0, len(source.Points))
				cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
				for _, point := range source.Points {
					if days <= 0 || point.Time.Before(cutoff) {
						continue
					}
					value := ltsLoadMetricValue(definition.key, point.Metrics)
					points = append(points, ltsMetricPoint{Time: point.Time, Value: &value, Count: point.TotalCount})
				}
				series = append(series, ltsMetricSeries{
					MetricKey: definition.key, EntityID: entityID, Type: "gauge", Unit: definition.unit,
					RetentionDays: float64(days), Downsampled: result.Sampled, DownsampleAlgorithm: "avg",
					IntervalSeconds: interval, Count: len(points), Points: points,
				})
			}
		case "gpu":
			tags := map[string]string{"device_index": strconv.Itoa(source.DeviceIndex)}
			if source.DeviceName != "" {
				tags["device_name"] = source.DeviceName
			}
			for _, definition := range ltsMetricDefinitions {
				if definition.source != "gpu" || !requested[definition.key] {
					continue
				}
				days := retention[definition.key]
				points := make([]ltsMetricPoint, 0, len(source.Points))
				cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
				for _, point := range source.Points {
					if days <= 0 || point.Time.Before(cutoff) {
						continue
					}
					value := ltsGPUMetricValue(definition.key, point.Metrics)
					points = append(points, ltsMetricPoint{Time: point.Time, Value: &value, Count: point.TotalCount})
				}
				series = append(series, ltsMetricSeries{
					MetricKey: definition.key, EntityID: entityID, Type: "gauge", Unit: definition.unit,
					RetentionDays: float64(days), Downsampled: result.Sampled, DownsampleAlgorithm: "avg",
					IntervalSeconds: interval, Tags: tags, Count: len(points), Points: points,
				})
			}
		}
	}
	return series
}

func ltsLoadMetricValue(key string, metrics map[string]float64) float64 {
	switch key {
	case "cpu.usage":
		return metrics["cpu"]
	case "gpu.usage":
		return metrics["gpu"]
	case "memory.used":
		return metrics["ram"]
	case "memory.total":
		return metrics["ram_total"]
	case "swap.used":
		return metrics["swap"]
	case "swap.total":
		return metrics["swap_total"]
	case "load.average":
		return metrics["load"]
	case "temperature":
		return metrics["temp"]
	case "disk.used":
		return metrics["disk"]
	case "disk.total":
		return metrics["disk_total"]
	case "net.in.rate":
		return metrics["net_in"]
	case "net.out.rate":
		return metrics["net_out"]
	case "net.total.up":
		return metrics["net_total_up"]
	case "net.total.down":
		return metrics["net_total_down"]
	case "traffic.up":
		return metrics["traffic_up"]
	case "traffic.down":
		return metrics["traffic_down"]
	case "process.count":
		return metrics["process"]
	case "connections.tcp":
		return math.Max(0, metrics["connections"]-metrics["connections_udp"])
	case "connections.udp":
		return metrics["connections_udp"]
	default:
		return 0
	}
}

func ltsGPUMetricValue(key string, metrics map[string]float64) float64 {
	switch key {
	case "gpu.device.usage":
		return metrics["utilization"]
	case "gpu.memory.used":
		return metrics["mem_used"]
	case "gpu.memory.total":
		return metrics["mem_total"]
	case "gpu.temperature":
		return metrics["temperature"]
	default:
		return 0
	}
}

func ltsPingMetricSeries(result *history.Response, requested map[string]bool, entityID string, retention map[string]int) []ltsMetricSeries {
	interval := ltsResolutionSeconds(result.Resolution)
	series := make([]ltsMetricSeries, 0, len(result.Series)*2)
	for _, source := range result.Series {
		if source.Kind != "ping" {
			continue
		}
		tags := map[string]string{"task_id": strconv.FormatUint(uint64(source.TaskID), 10)}
		for _, definition := range ltsMetricDefinitions {
			if definition.source != "ping" || !requested[definition.key] {
				continue
			}
			retentionDays := retention[definition.key]
			cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
			points := make([]ltsMetricPoint, 0, len(source.Points))
			for _, point := range source.Points {
				if retentionDays <= 0 || point.Time.Before(cutoff) {
					continue
				}
				var value *float64
				switch definition.key {
				case "ping.latency_ms":
					if point.TotalCount > point.LossCount {
						average := point.Avg
						value = &average
					}
				case "ping.loss":
					if point.TotalCount > 0 {
						loss := float64(point.LossCount) / float64(point.TotalCount)
						value = &loss
					}
				}
				points = append(points, ltsMetricPoint{Time: point.Time, Value: value, Count: point.TotalCount})
			}
			series = append(series, ltsMetricSeries{
				MetricKey: definition.key, EntityID: entityID, Type: "gauge", Unit: definition.unit,
				RetentionDays: float64(retentionDays), Downsampled: result.Sampled, DownsampleAlgorithm: "avg",
				IntervalSeconds: interval, Tags: tags, Count: len(points), Points: points,
			})
		}
	}
	return series
}

func ltsResolutionSeconds(value string) int64 {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return int64(duration / time.Second)
}

func limitLTSMetricPoints(series []ltsMetricSeries, maxPoints int) []ltsMetricSeries {
	total := 0
	var start, end time.Time
	for index := range series {
		total += len(series[index].Points)
		if len(series[index].Points) > 0 {
			first := series[index].Points[0].Time
			last := series[index].Points[len(series[index].Points)-1].Time
			if start.IsZero() || first.Before(start) {
				start = first
			}
			if end.IsZero() || last.After(end) {
				end = last
			}
		}
	}
	if total <= maxPoints {
		return series
	}

	// Compute aligned re-bucketing interval.
	perSeriesBudget := maxPoints / len(series)
	if perSeriesBudget < 1 {
		perSeriesBudget = 1
	}
	rangeDuration := end.Sub(start)
	if rangeDuration <= 0 {
		rangeDuration = time.Hour
	}
	targetInterval := ltsChooseBucketSize(rangeDuration / time.Duration(perSeriesBudget))

	// Re-bucket all series onto the same aligned time grid.
	for index := range series {
		series[index].Points, series[index].IntervalSeconds = rebucketLTSMetricPoints(
			series[index].Points,
			targetInterval,
		)
	}

	return series
}

func ltsChooseBucketSize(minimum time.Duration) time.Duration {
	candidates := []time.Duration{
		10 * time.Second, 30 * time.Second, time.Minute,
		5 * time.Minute, 10 * time.Minute, 15 * time.Minute,
		30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour,
		12 * time.Hour, 24 * time.Hour,
	}
	for _, candidate := range candidates {
		if candidate >= minimum {
			return candidate
		}
	}
	return 24 * time.Hour
}

func rebucketLTSMetricPoints(points []ltsMetricPoint, interval time.Duration) ([]ltsMetricPoint, int64) {
	if len(points) == 0 || interval <= 0 {
		return points, 0
	}

	buckets := make(map[int64]*ltsMetricPoint)
	for _, point := range points {
		bucket := point.Time.Truncate(interval).Unix()
		merged := buckets[bucket]
		if merged == nil {
			merged = &ltsMetricPoint{
				Time:  point.Time.Truncate(interval),
				Count: 0,
			}
			buckets[bucket] = merged
		}

		if point.Value != nil && merged.Value == nil {
			merged.Value = new(float64)
			*merged.Value = 0
		}
		if point.Value != nil {
			*merged.Value = (*merged.Value*float64(merged.Count) + *point.Value*float64(point.Count)) / float64(merged.Count+point.Count)
		}
		merged.Count += point.Count
	}

	result := make([]ltsMetricPoint, 0, len(buckets))
	for _, merged := range buckets {
		result = append(result, *merged)
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Time.Before(result[j].Time) })
	return result, int64(interval / time.Second)
}

type ltsPingStat struct {
	EntityID        string            `json:"entity_id"`
	TaskID          string            `json:"task_id"`
	Name            string            `json:"name,omitempty"`
	Type            string            `json:"type,omitempty"`
	Interval        int               `json:"interval,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	Total           int               `json:"total"`
	Valid           int               `json:"valid"`
	Loss            float64           `json:"loss"`
	LossApproximate bool              `json:"loss_approximate"`
	Min             *float64          `json:"min"`
	Max             *float64          `json:"max"`
	Avg             *float64          `json:"avg"`
	Latest          *float64          `json:"latest"`
	P50             *float64          `json:"p50"`
	P99             *float64          `json:"p99"`
	Stddev          *float64          `json:"stddev"`
	P99P50Ratio     float64           `json:"p99_p50_ratio"`
}

func publicGetPingMetricStats(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params ltsMetricQueryParams
	if err := req.BindParams(&params); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid Ping statistics query: "+err.Error(), nil)
	}
	requestedEntity := params.EntityID
	if requestedEntity == "" {
		requestedEntity = params.UUID
	}
	entityIDs, entityErr := ltsMetricEntityIDs(ctx, requestedEntity, params.EntityIDs)
	if entityErr != nil {
		return nil, entityErr
	}
	baseRequest, err := ltsMetricHistoryRequest(params)
	if err != nil {
		return nil, historyRPCError(err, "Invalid Ping statistics query")
	}
	retentionDays := ltsMetricRetentionDays()["ping.latency_ms"]
	maxPoints := ltsMetricMaxPoints(params.MaxPoints)
	if len(entityIDs) > 0 {
		baseRequest.MaxPoints = max(1, maxPoints/len(entityIDs))
	}
	ctx, cancel := context.WithTimeout(ctx, history.QueryTimeout)
	defer cancel()

	taskList, err := tasks.GetAllPingTasks()
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to load Ping tasks: "+err.Error(), nil)
	}
	taskMap := make(map[uint]struct {
		name     string
		taskType string
		interval int
	}, len(taskList))
	for _, task := range taskList {
		taskMap[task.Id] = struct {
			name     string
			taskType string
			interval int
		}{task.Name, task.Type, task.Interval}
	}

	stats := make([]ltsPingStat, 0)
	var responseStart, responseEnd time.Time
	var intervalSeconds int64
	for _, entityID := range entityIDs {
		queryRequest := baseRequest
		queryRequest.Type = "ping"
		queryRequest.UUID = entityID
		queryRequest, ok := clampLTSHistoryRequest(queryRequest, retentionDays)
		if !ok {
			continue
		}
		result, queryErr := history.Query(ctx, queryRequest)
		if queryErr != nil {
			return nil, historyRPCError(queryErr, "Failed to query Ping statistics")
		}
		if responseStart.IsZero() || result.Start.Before(responseStart) {
			responseStart = result.Start
		}
		if responseEnd.IsZero() || result.End.After(responseEnd) {
			responseEnd = result.End
		}
		intervalSeconds = max(intervalSeconds, ltsResolutionSeconds(result.Resolution))
		for _, source := range result.Series {
			if source.Kind != "ping" {
				continue
			}
			stat := ltsPingStat{
				EntityID: entityID, TaskID: strconv.FormatUint(uint64(source.TaskID), 10),
				Tags: map[string]string{"task_id": strconv.FormatUint(uint64(source.TaskID), 10)},
			}
			if task, ok := taskMap[source.TaskID]; ok {
				stat.Name, stat.Type, stat.Interval = task.name, task.taskType, task.interval
			}
			summary := source.PingSummary
			if summary == nil {
				summary = &history.PingSummary{}
			}
			stat.Total, stat.Valid = summary.TotalCount, summary.ValidCount
			stat.LossApproximate = summary.Approximate
			if stat.Total > 0 {
				stat.Loss = float64(stat.Total-stat.Valid) / float64(stat.Total) * 100
			}
			if stat.Valid > 0 {
				minimum, maximum, average := summary.Min, summary.Max, summary.Avg
				latest, standardDeviation := summary.Latest, summary.Stddev
				p50, p99 := summary.P50, summary.P99
				stat.Min, stat.Max, stat.Latest = &minimum, &maximum, &latest
				stat.Avg, stat.Stddev, stat.P50, stat.P99 = &average, &standardDeviation, &p50, &p99
				if p50 > 0 && p99 >= p50 {
					base := math.Max(10, math.Min(50, p50))
					stat.P99P50Ratio = (p99 - p50) / base
				}
			}
			stats = append(stats, stat)
		}
	}
	if responseStart.IsZero() {
		responseStart, responseEnd, err = ltsMetricRange(params)
		if err != nil {
			return nil, historyRPCError(err, "Invalid Ping statistics query")
		}
	}
	return map[string]any{
		"start": responseStart, "end": responseEnd, "interval_seconds": intervalSeconds,
		"stats": stats, "count": len(stats),
	}, nil
}
