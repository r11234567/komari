// Package history provides bounded, cancellable history queries shared by all themes.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

const (
	DefaultMaxPoints = 1500
	MaxMaxPoints     = 5000
	QueryTimeout     = 20 * time.Second
)

var (
	ErrInvalidQuery  = errors.New("history query requires a supported type and uuid or task_id")
	ErrRangeTooLarge = errors.New("history range must be positive and no longer than 90 days")
	queryGroup       singleflight.Group
)

type QueryRequest struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	TaskID    *uint  `json:"task_id,omitempty"`
	Start     string `json:"start,omitempty"`
	End       string `json:"end,omitempty"`
	Hours     int    `json:"hours,omitempty"`
	MaxPoints int    `json:"max_points,omitempty"`
}

type Point struct {
	Time       time.Time          `json:"time"`
	Avg        float64            `json:"avg,omitempty"`
	Min        float64            `json:"min,omitempty"`
	Max        float64            `json:"max,omitempty"`
	LossCount  int                `json:"loss_count,omitempty"`
	TotalCount int                `json:"total_count"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
}

type Series struct {
	Kind        string       `json:"kind"`
	Client      string       `json:"client,omitempty"`
	TaskID      uint         `json:"task_id,omitempty"`
	DeviceIndex int          `json:"device_index,omitempty"`
	DeviceName  string       `json:"device_name,omitempty"`
	PingSummary *PingSummary `json:"ping_summary,omitempty"`
	Points      []Point      `json:"points"`
}

type PingSummary struct {
	TotalCount  int     `json:"total_count"`
	LossCount   int     `json:"loss_count"`
	ValidCount  int     `json:"valid_count"`
	Avg         float64 `json:"avg,omitempty"`
	Min         float64 `json:"min,omitempty"`
	Max         float64 `json:"max,omitempty"`
	Latest      float64 `json:"latest,omitempty"`
	P50         float64 `json:"p50,omitempty"`
	P99         float64 `json:"p99,omitempty"`
	Stddev      float64 `json:"stddev,omitempty"`
	Approximate bool    `json:"approximate"`
}

type Response struct {
	Type           string    `json:"type"`
	Start          time.Time `json:"start"`
	End            time.Time `json:"end"`
	Resolution     string    `json:"resolution"`
	RawCount       int64     `json:"raw_count"`
	ReturnedPoints int       `json:"returned_points"`
	Sampled        bool      `json:"sampled"`
	Series         []Series  `json:"series"`
}

type pingKey struct {
	client string
	taskID uint
	bucket time.Time
}

type loadBucket struct {
	time    time.Time
	count   int
	metrics map[string]float64
}

type gpuKey struct {
	device int
	name   string
	bucket time.Time
}

func parseRequest(req QueryRequest) (time.Time, time.Time, time.Duration, int, error) {
	if req.Type != "load" && req.Type != "ping" {
		return time.Time{}, time.Time{}, 0, 0, ErrInvalidQuery
	}
	if req.Type == "load" && req.UUID == "" {
		return time.Time{}, time.Time{}, 0, 0, ErrInvalidQuery
	}
	if req.Type == "ping" && req.UUID == "" && req.TaskID == nil {
		return time.Time{}, time.Time{}, 0, 0, ErrInvalidQuery
	}

	end := time.Now()
	var err error
	if req.End != "" {
		end, err = time.Parse(time.RFC3339, req.End)
		if err != nil {
			return time.Time{}, time.Time{}, 0, 0, err
		}
	}
	start := end.Add(-4 * time.Hour)
	if req.Start != "" {
		start, err = time.Parse(time.RFC3339, req.Start)
		if err != nil {
			return time.Time{}, time.Time{}, 0, 0, err
		}
	} else if req.Hours > 90*24 {
		return time.Time{}, time.Time{}, 0, 0, ErrRangeTooLarge
	} else if req.Hours > 0 {
		start = end.Add(-time.Duration(req.Hours) * time.Hour)
	}
	if !end.After(start) || end.Sub(start) > 90*24*time.Hour {
		return time.Time{}, time.Time{}, 0, 0, ErrRangeTooLarge
	}

	maxPoints := req.MaxPoints
	if maxPoints <= 0 {
		maxPoints = DefaultMaxPoints
	}
	if maxPoints > MaxMaxPoints {
		maxPoints = MaxMaxPoints
	}
	bucketSize := chooseBucketSize(end.Sub(start) / time.Duration(maxPoints))
	return start, end, bucketSize, maxPoints, nil
}

func chooseBucketSize(minimum time.Duration) time.Duration {
	for _, candidate := range []time.Duration{
		10 * time.Second, 30 * time.Second, time.Minute,
		5 * time.Minute, 10 * time.Minute, 15 * time.Minute,
		30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour,
		12 * time.Hour, 24 * time.Hour,
	} {
		if candidate >= minimum {
			return candidate
		}
	}
	return 24 * time.Hour
}

func Query(ctx context.Context, req QueryRequest) (*Response, error) {
	key, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	result := queryGroup.DoChan(string(key), func() (any, error) {
		return query(context.WithoutCancel(ctx), req)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case shared := <-result:
		if shared.Err != nil {
			return nil, shared.Err
		}
		return shared.Val.(*Response), nil
	}
}

func query(ctx context.Context, req QueryRequest) (*Response, error) {
	start, end, bucketSize, maxPoints, err := parseRequest(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()

	response := &Response{
		Type:       req.Type,
		Start:      start,
		End:        end,
		Resolution: bucketSize.String(),
	}
	if req.Type == "ping" {
		response.Series, response.RawCount, err = queryPing(ctx, req, start, end, bucketSize, maxPoints)
	} else {
		response.Series, response.RawCount, err = queryLoad(ctx, req, start, end, bucketSize, maxPoints)
	}
	if err != nil {
		return nil, err
	}
	limitedSeries, effectiveResolution := limitTotalPoints(response.Series, maxPoints)
	response.Series = limitedSeries
	if effectiveResolution > bucketSize {
		response.Resolution = effectiveResolution.String()
	}
	for _, series := range response.Series {
		response.ReturnedPoints += len(series.Points)
	}
	response.Sampled = response.RawCount > int64(response.ReturnedPoints)
	return response, nil
}

func queryPing(ctx context.Context, req QueryRequest, start, end time.Time, size time.Duration, maxPoints int) ([]Series, int64, error) {
	buckets := make(map[pingKey]*Point)
	count, err := queryRawPingBuckets(ctx, req, start, end, size, buckets)
	if err != nil {
		return nil, 0, err
	}

	rollupQuery := dbcore.GetReadDBInstance().WithContext(ctx).
		Model(&models.PingRollup{}).
		Where("time >= ? AND time <= ?", start, end)
	if req.UUID != "" {
		rollupQuery = rollupQuery.Where("client = ?", req.UUID)
	}
	if req.TaskID != nil {
		rollupQuery = rollupQuery.Where("task_id = ?", *req.TaskID)
	}
	var rollups []models.PingRollup
	if err := rollupQuery.Order("time ASC").Find(&rollups).Error; err != nil {
		return nil, 0, err
	}
	for _, rollup := range rollups {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		key := pingKey{client: rollup.Client, taskID: rollup.TaskID, bucket: rollup.Time.ToTime().Truncate(size)}
		mergePingBucket(buckets, key, int(rollup.TotalCount), int(rollup.LossCount), rollup.ValueSum, rollup.Minimum, rollup.Maximum)
		count += rollup.TotalCount
	}

	seriesMap := make(map[string]*Series)
	for key, point := range buckets {
		valid := point.TotalCount - point.LossCount
		if valid > 0 {
			point.Avg /= float64(valid)
		}
		seriesKey := key.client + "\x00" + strconv.FormatUint(uint64(key.taskID), 10)
		series := seriesMap[seriesKey]
		if series == nil {
			series = &Series{Kind: "ping", Client: key.client, TaskID: key.taskID}
			seriesMap[seriesKey] = series
		}
		series.Points = append(series.Points, *point)
	}
	return limitSeries(seriesMap, maxPoints), count, nil
}

func queryRawPingBuckets(ctx context.Context, req QueryRequest, start, end time.Time, size time.Duration, buckets map[pingKey]*Point) (int64, error) {
	db := dbcore.GetReadDBInstance().WithContext(ctx).
		Model(&models.PingRecord{}).
		Where("time >= ? AND time <= ?", start, end)
	if req.UUID != "" {
		db = db.Where("client = ?", req.UUID)
	}
	if req.TaskID != nil {
		db = db.Where("task_id = ?", *req.TaskID)
	}
	if db.Dialector.Name() == "sqlite" {
		return queryRawPingBucketsSQLite(ctx, db, size, buckets)
	}

	rows, err := db.Select("client, task_id, time, value").Order("time ASC").Rows()
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var count int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var client string
		var taskID uint
		var recorded models.LocalTime
		var value int
		if err := rows.Scan(&client, &taskID, &recorded, &value); err != nil {
			return 0, err
		}
		count++
		key := pingKey{client: client, taskID: taskID, bucket: recorded.ToTime().Truncate(size)}
		if value < 0 {
			mergePingBucket(buckets, key, 1, 1, 0, 0, 0)
			continue
		}
		mergePingBucket(buckets, key, 1, 0, float64(value), float64(value), float64(value))
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

func queryRawPingBucketsSQLite(ctx context.Context, db *gorm.DB, size time.Duration, buckets map[pingKey]*Point) (int64, error) {
	bucketSeconds := int64(size / time.Second)
	rows, err := db.Select(`
		client,
		task_id,
		datetime((CAST(strftime('%s', time) AS INTEGER) / ?) * ?, 'unixepoch') AS bucket_time,
		COUNT(*) AS total_count,
		SUM(CASE WHEN value < 0 THEN 1 ELSE 0 END) AS loss_count,
		SUM(CASE WHEN value >= 0 THEN value ELSE 0 END) AS value_sum,
		COALESCE(MIN(CASE WHEN value >= 0 THEN value END), 0) AS minimum,
		COALESCE(MAX(CASE WHEN value >= 0 THEN value END), 0) AS maximum`,
		bucketSeconds, bucketSeconds).
		Group("client, task_id, bucket_time").
		Order("bucket_time ASC").
		Rows()
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var count int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var client string
		var taskID uint
		var recorded models.LocalTime
		var total, loss int
		var valueSum, minimum, maximum float64
		if err := rows.Scan(&client, &taskID, &recorded, &total, &loss, &valueSum, &minimum, &maximum); err != nil {
			return 0, err
		}
		key := pingKey{client: client, taskID: taskID, bucket: recorded.ToTime()}
		mergePingBucket(buckets, key, total, loss, valueSum, minimum, maximum)
		count += int64(total)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

func mergePingBucket(buckets map[pingKey]*Point, key pingKey, total, loss int, valueSum, minimum, maximum float64) {
	point := buckets[key]
	if point == nil {
		point = &Point{Time: key.bucket}
		buckets[key] = point
	}
	existingValid := point.TotalCount - point.LossCount
	incomingValid := total - loss
	if incomingValid > 0 {
		if existingValid == 0 || minimum < point.Min {
			point.Min = minimum
		}
		if existingValid == 0 || maximum > point.Max {
			point.Max = maximum
		}
	}
	point.TotalCount += total
	point.LossCount += loss
	point.Avg += valueSum
}

func queryLoad(ctx context.Context, req QueryRequest, start, end time.Time, size time.Duration, maxPoints int) ([]Series, int64, error) {
	if req.UUID == "" {
		return nil, 0, errors.New("load history requires uuid")
	}
	buckets := make(map[time.Time]*loadBucket)
	var count int64
	for _, table := range []string{"records_long_term", "records"} {
		db := dbcore.GetReadDBInstance().WithContext(ctx).Table(table).
			Where("client = ? AND time >= ? AND time <= ?", req.UUID, start, end)
		rows, err := db.Order("time ASC").Rows()
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return nil, 0, err
			}
			var record models.Record
			if err := db.ScanRows(rows, &record); err != nil {
				rows.Close()
				return nil, 0, err
			}
			count++
			bucketTime := record.Time.ToTime().Truncate(size)
			bucket := buckets[bucketTime]
			if bucket == nil {
				bucket = &loadBucket{time: bucketTime, metrics: make(map[string]float64)}
				buckets[bucketTime] = bucket
			}
			bucket.count++
			addLoadMetrics(bucket.metrics, record)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, 0, err
		}
		rows.Close()
	}
	var rollups []models.ResourceRollup
	if err := dbcore.GetReadDBInstance().WithContext(ctx).
		Where("client = ? AND time >= ? AND time <= ?", req.UUID, start, end).
		Order("time ASC").Find(&rollups).Error; err != nil {
		return nil, 0, err
	}
	for _, record := range rollups {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		bucketTime := record.Time.ToTime().Truncate(size)
		bucket := buckets[bucketTime]
		if bucket == nil {
			bucket = &loadBucket{time: bucketTime, metrics: make(map[string]float64)}
			buckets[bucketTime] = bucket
		}
		bucket.count += int(record.Count)
		for name, sum := range record.Sums {
			bucket.metrics[name] += sum
		}
		count += record.Count
	}

	points := averageBuckets(buckets, maxPoints)
	gpuSeries, gpuCount, err := queryGPU(ctx, req.UUID, start, end, size, maxPoints)
	if err != nil {
		return nil, 0, err
	}
	series := []Series{{Kind: "load", Client: req.UUID, Points: points}}
	series = append(series, gpuSeries...)
	return series, count + gpuCount, nil
}

func addLoadMetrics(metrics map[string]float64, record models.Record) {
	metrics["cpu"] += float64(record.Cpu)
	metrics["gpu"] += float64(record.Gpu)
	metrics["ram"] += float64(record.Ram)
	metrics["ram_total"] += float64(record.RamTotal)
	metrics["swap"] += float64(record.Swap)
	metrics["swap_total"] += float64(record.SwapTotal)
	metrics["load"] += float64(record.Load)
	metrics["temp"] += float64(record.Temp)
	metrics["disk"] += float64(record.Disk)
	metrics["disk_total"] += float64(record.DiskTotal)
	metrics["net_in"] += float64(record.NetIn)
	metrics["net_out"] += float64(record.NetOut)
	metrics["net_total_up"] += float64(record.NetTotalUp)
	metrics["net_total_down"] += float64(record.NetTotalDown)
	metrics["traffic_up"] += float64(record.TrafficUp)
	metrics["traffic_down"] += float64(record.TrafficDown)
	metrics["process"] += float64(record.Process)
	metrics["connections"] += float64(record.Connections)
	metrics["connections_udp"] += float64(record.ConnectionsUdp)
}

func queryGPU(ctx context.Context, uuid string, start, end time.Time, size time.Duration, maxPoints int) ([]Series, int64, error) {
	buckets := make(map[gpuKey]*loadBucket)
	var count int64
	for _, table := range []string{"gpu_records_long_term", "gpu_records"} {
		db := dbcore.GetReadDBInstance().WithContext(ctx).Table(table).
			Where("client = ? AND time >= ? AND time <= ?", uuid, start, end)
		rows, err := db.Order("time ASC").Rows()
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return nil, 0, err
			}
			var record models.GPURecord
			if err := db.ScanRows(rows, &record); err != nil {
				rows.Close()
				return nil, 0, err
			}
			count++
			key := gpuKey{device: record.DeviceIndex, name: record.DeviceName, bucket: record.Time.ToTime().Truncate(size)}
			bucket := buckets[key]
			if bucket == nil {
				bucket = &loadBucket{time: key.bucket, metrics: make(map[string]float64)}
				buckets[key] = bucket
			}
			bucket.count++
			bucket.metrics["mem_total"] += float64(record.MemTotal)
			bucket.metrics["mem_used"] += float64(record.MemUsed)
			bucket.metrics["utilization"] += float64(record.Utilization)
			bucket.metrics["temperature"] += float64(record.Temperature)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, 0, err
		}
		rows.Close()
	}
	var rollups []models.GPURollup
	if err := dbcore.GetReadDBInstance().WithContext(ctx).
		Where("client = ? AND time >= ? AND time <= ?", uuid, start, end).
		Order("time ASC").Find(&rollups).Error; err != nil {
		return nil, 0, err
	}
	for _, record := range rollups {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		key := gpuKey{device: record.DeviceIndex, name: record.DeviceName, bucket: record.Time.ToTime().Truncate(size)}
		bucket := buckets[key]
		if bucket == nil {
			bucket = &loadBucket{time: key.bucket, metrics: make(map[string]float64)}
			buckets[key] = bucket
		}
		bucket.count += int(record.Count)
		for name, sum := range record.Sums {
			bucket.metrics[name] += sum
		}
		count += record.Count
	}

	byDevice := make(map[string]*Series)
	for key, bucket := range buckets {
		deviceKey := strconv.Itoa(key.device) + "\x00" + key.name
		series := byDevice[deviceKey]
		if series == nil {
			series = &Series{Kind: "gpu", Client: uuid, DeviceIndex: key.device, DeviceName: key.name}
			byDevice[deviceKey] = series
		}
		point := Point{Time: bucket.time, TotalCount: bucket.count, Metrics: make(map[string]float64)}
		for name, sum := range bucket.metrics {
			point.Metrics[name] = sum / float64(bucket.count)
		}
		series.Points = append(series.Points, point)
	}

	result := make([]Series, 0, len(byDevice))
	for _, series := range byDevice {
		sort.Slice(series.Points, func(i, j int) bool { return series.Points[i].Time.Before(series.Points[j].Time) })
		if len(series.Points) > maxPoints {
			series.Points = series.Points[len(series.Points)-maxPoints:]
		}
		result = append(result, *series)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].DeviceIndex < result[j].DeviceIndex })
	return result, count, nil
}

func averageBuckets(buckets map[time.Time]*loadBucket, maxPoints int) []Point {
	points := make([]Point, 0, len(buckets))
	for _, bucket := range buckets {
		point := Point{Time: bucket.time, TotalCount: bucket.count, Metrics: make(map[string]float64)}
		for name, sum := range bucket.metrics {
			point.Metrics[name] = sum / float64(bucket.count)
		}
		points = append(points, point)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Time.Before(points[j].Time) })
	if len(points) > maxPoints {
		points = points[len(points)-maxPoints:]
	}
	return points
}

func limitSeries(seriesMap map[string]*Series, maxPoints int) []Series {
	series := make([]Series, 0, len(seriesMap))
	for _, item := range seriesMap {
		sort.Slice(item.Points, func(i, j int) bool { return item.Points[i].Time.Before(item.Points[j].Time) })
		item.PingSummary = summarizePingPoints(item.Points)
		if len(item.Points) > maxPoints {
			item.Points = item.Points[len(item.Points)-maxPoints:]
		}
		series = append(series, *item)
	}
	sort.Slice(series, func(i, j int) bool {
		if series[i].Client == series[j].Client {
			return series[i].TaskID < series[j].TaskID
		}
		return series[i].Client < series[j].Client
	})
	return series
}

type weightedPingPoint struct {
	value  float64
	weight int
}

func summarizePingPoints(points []Point) *PingSummary {
	summary := &PingSummary{Approximate: true}
	weighted := make([]weightedPingPoint, 0, len(points))
	var sum, squared float64
	for _, point := range points {
		summary.TotalCount += point.TotalCount
		summary.LossCount += point.LossCount
		valid := point.TotalCount - point.LossCount
		summary.ValidCount += valid
		if valid == 0 {
			continue
		}
		sum += point.Avg * float64(valid)
		squared += point.Avg * point.Avg * float64(valid)
		weighted = append(weighted, weightedPingPoint{value: point.Avg, weight: valid})
		if summary.ValidCount == valid || point.Min < summary.Min {
			summary.Min = point.Min
		}
		if summary.ValidCount == valid || point.Max > summary.Max {
			summary.Max = point.Max
		}
		summary.Latest = point.Avg
	}
	if summary.ValidCount == 0 {
		return summary
	}
	summary.Avg = sum / float64(summary.ValidCount)
	variance := math.Max(0, squared/float64(summary.ValidCount)-summary.Avg*summary.Avg)
	summary.Stddev = math.Sqrt(variance)
	sort.Slice(weighted, func(i, j int) bool { return weighted[i].value < weighted[j].value })
	summary.P50 = weightedPingPointPercentile(weighted, 0.50)
	summary.P99 = weightedPingPointPercentile(weighted, 0.99)
	return summary
}

func weightedPingPointPercentile(values []weightedPingPoint, percentile float64) float64 {
	total := 0
	for _, item := range values {
		total += item.weight
	}
	target := int(math.Ceil(percentile * float64(total)))
	if target < 1 {
		target = 1
	}
	seen := 0
	for _, item := range values {
		seen += item.weight
		if seen >= target {
			return item.value
		}
	}
	return values[len(values)-1].value
}

func limitTotalPoints(series []Series, maxPoints int) ([]Series, time.Duration) {
	if maxPoints <= 0 || len(series) == 0 {
		return series, 0
	}
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
		return series, 0
	}

	// Compute the effective bucket size needed to stay within budget while keeping all series aligned.
	perSeriesBudget := maxPoints / len(series)
	if perSeriesBudget < 1 {
		perSeriesBudget = 1
	}
	rangeDuration := end.Sub(start)
	if rangeDuration <= 0 {
		rangeDuration = time.Hour
	}
	targetInterval := chooseBucketSize(rangeDuration / time.Duration(perSeriesBudget))

	// Re-bucket all series onto the same aligned time grid.
	for index := range series {
		series[index].Points = rebucketPoints(series[index].Points, targetInterval)
	}

	// Safety: if still over budget, coarsen further up to 24h, then trim newest if needed.
	for attempt := 0; attempt < 5; attempt++ {
		total = 0
		for index := range series {
			total += len(series[index].Points)
		}
		if total <= maxPoints {
			break
		}
		nextInterval := chooseBucketSize(targetInterval + 1)
		if nextInterval == targetInterval || nextInterval > 24*time.Hour {
			break
		}
		targetInterval = nextInterval
		for index := range series {
			series[index].Points = rebucketPoints(series[index].Points, targetInterval)
		}
	}

	// Final fallback: trim newest points if still over.
	total = 0
	for index := range series {
		total += len(series[index].Points)
	}
	if total > maxPoints {
		remainingPoints := maxPoints
		remainingSeries := len(series)
		for index := range series {
			if remainingPoints == 0 {
				series[index].Points = nil
				continue
			}
			budget := remainingPoints / remainingSeries
			if budget == 0 {
				budget = 1
			}
			if budget < len(series[index].Points) {
				series[index].Points = series[index].Points[len(series[index].Points)-budget:]
			}
			remainingPoints -= len(series[index].Points)
			remainingSeries--
		}
	}

	return series, targetInterval
}

// rebucketPoints re-aggregates points onto a coarser aligned time grid.
// For ping points, it computes count-weighted averages and preserves min/max.
// For load/GPU points, it averages the metrics map by count.
func rebucketPoints(points []Point, interval time.Duration) []Point {
	if len(points) == 0 || interval <= 0 {
		return points
	}

	buckets := make(map[int64]*Point)
	for _, point := range points {
		bucket := point.Time.Truncate(interval).Unix()
		merged := buckets[bucket]
		if merged == nil {
			merged = &Point{
				Time:    point.Time.Truncate(interval),
				Metrics: make(map[string]float64),
			}
			buckets[bucket] = merged
		}

		merged.TotalCount += point.TotalCount
		merged.LossCount += point.LossCount
		validCount := point.TotalCount - point.LossCount
		if validCount > 0 {
			mergedValid := merged.TotalCount - merged.LossCount
			if mergedValid == validCount {
				// First valid contribution to this bucket.
				merged.Avg = point.Avg
				merged.Min = point.Min
				merged.Max = point.Max
			} else {
				// Merge weighted average.
				prevValid := mergedValid - validCount
				merged.Avg = (merged.Avg*float64(prevValid) + point.Avg*float64(validCount)) / float64(mergedValid)
				if point.Min < merged.Min {
					merged.Min = point.Min
				}
				if point.Max > merged.Max {
					merged.Max = point.Max
				}
			}
		}

		// Merge metrics map (for load/GPU).
		for key, value := range point.Metrics {
			merged.Metrics[key] += value * float64(point.TotalCount)
		}
	}

	// Finalize metrics averages.
	result := make([]Point, 0, len(buckets))
	for _, merged := range buckets {
		if merged.TotalCount > 0 {
			for key := range merged.Metrics {
				merged.Metrics[key] /= float64(merged.TotalCount)
			}
		}
		result = append(result, *merged)
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Time.Before(result[j].Time) })
	return result
}
