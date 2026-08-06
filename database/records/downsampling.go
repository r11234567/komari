package records

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

const downsamplingWindowsPerPass = 32

// MaintainRecords keeps the legacy compactor as the disabled-policy fallback.
func MaintainRecords() error {
	policy, err := GetDownsamplingPolicy()
	if err != nil {
		return err
	}
	if !policy.Enabled {
		return CompactRecord()
	}
	return downsampleRecords(policy, time.Now())
}

func downsampleRecords(policy DownsamplingPolicy, now time.Time) error {
	parsed, err := parseDownsamplingPolicy(policy)
	if err != nil {
		return err
	}

	_, err = dbcore.TryMaintenance(context.Background(), func(db *gorm.DB) error {
		for index, tier := range parsed.tiers {
			sourceResolution := time.Duration(0)
			sourceRetention := parsed.rawRetention
			if index > 0 {
				sourceResolution = parsed.tiers[index-1].interval
				sourceRetention = parsed.tiers[index-1].retention
			}
			eligibleEnd := now.Add(-sourceRetention).Truncate(tier.interval)
			for pass := 0; pass < downsamplingWindowsPerPass; pass++ {
				progressed, compactErr := compactResourceWindow(db, sourceResolution, tier.interval, eligibleEnd)
				if compactErr != nil {
					return fmt.Errorf("compact resource tier %d: %w", index+1, compactErr)
				}
				if !progressed {
					break
				}
			}
			for pass := 0; pass < downsamplingWindowsPerPass; pass++ {
				progressed, compactErr := compactGPUWindow(db, sourceResolution, tier.interval, eligibleEnd)
				if compactErr != nil {
					return fmt.Errorf("compact GPU tier %d: %w", index+1, compactErr)
				}
				if !progressed {
					break
				}
			}
			for pass := 0; pass < downsamplingWindowsPerPass; pass++ {
				progressed, compactErr := compactPingWindow(db, sourceResolution, tier.interval, eligibleEnd)
				if compactErr != nil {
					return fmt.Errorf("compact Ping tier %d: %w", index+1, compactErr)
				}
				if !progressed {
					break
				}
			}
		}

		terminal := parsed.tiers[len(parsed.tiers)-1]
		if err := deleteRollupsBefore(db, "resource_rollups", terminal.interval, now.Add(-terminal.retention)); err != nil {
			return err
		}
		if err := deleteRollupsBefore(db, "gpu_rollups", terminal.interval, now.Add(-terminal.retention)); err != nil {
			return err
		}
		return deleteRollupsBefore(db, "ping_rollups", terminal.interval, now.Add(-terminal.retention))
	})
	return err
}

type downsamplingTimeSample struct {
	Time models.LocalTime
}

func oldestSourceTime(db *gorm.DB, tables []string, resolution time.Duration, before time.Time) (time.Time, error) {
	var oldest time.Time
	for _, table := range tables {
		var sample downsamplingTimeSample
		query := db.Table(table).Select("time").Where("time < ?", before)
		if resolution > 0 {
			query = query.Where("resolution_seconds = ?", int64(resolution/time.Second))
		}
		if err := query.Order("time ASC").Limit(1).Scan(&sample).Error; err != nil {
			return time.Time{}, err
		}
		candidate := sample.Time.ToTime()
		if !candidate.IsZero() && (oldest.IsZero() || candidate.Before(oldest)) {
			oldest = candidate
		}
	}
	return oldest, nil
}

func compactResourceWindow(db *gorm.DB, sourceResolution, targetResolution time.Duration, eligibleEnd time.Time) (bool, error) {
	tables := []string{"resource_rollups"}
	if sourceResolution == 0 {
		tables = []string{"records", "records_long_term"}
	}
	oldest, err := oldestSourceTime(db, tables, sourceResolution, eligibleEnd)
	if err != nil || oldest.IsZero() {
		return false, err
	}
	windowStart := oldest.Truncate(targetResolution)
	windowEnd := windowStart.Add(targetResolution)
	if windowEnd.After(eligibleEnd) {
		return false, nil
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		buckets := make(map[string]*models.ResourceRollup)
		if sourceResolution == 0 {
			var raw []models.Record
			if err := tx.Table("records").Where("time >= ? AND time < ?", windowStart, windowEnd).Find(&raw).Error; err != nil {
				return err
			}
			var legacy []models.Record
			if err := tx.Table("records_long_term").Where("time >= ? AND time < ?", windowStart, windowEnd).Find(&legacy).Error; err != nil {
				return err
			}
			rawClients := make(map[string]struct{}, len(raw))
			for _, record := range raw {
				rawClients[record.Client] = struct{}{}
			}
			for _, record := range legacy {
				if _, hasRaw := rawClients[record.Client]; !hasRaw {
					raw = append(raw, record)
				}
			}
			for _, record := range raw {
				bucket := buckets[record.Client]
				if bucket == nil {
					bucket = &models.ResourceRollup{
						Client: record.Client, Time: models.FromTime(windowStart),
						ResolutionSeconds: int64(targetResolution / time.Second), Sums: models.MetricSums{},
					}
					buckets[record.Client] = bucket
				}
				bucket.Count++
				addRecordSums(bucket.Sums, record, 1)
			}
		} else {
			var source []models.ResourceRollup
			if err := tx.Where("resolution_seconds = ? AND time >= ? AND time < ?", int64(sourceResolution/time.Second), windowStart, windowEnd).Find(&source).Error; err != nil {
				return err
			}
			for _, record := range source {
				bucket := buckets[record.Client]
				if bucket == nil {
					bucket = &models.ResourceRollup{
						Client: record.Client, Time: models.FromTime(windowStart),
						ResolutionSeconds: int64(targetResolution / time.Second), Sums: models.MetricSums{},
					}
					buckets[record.Client] = bucket
				}
				bucket.Count += record.Count
				mergeMetricSums(bucket.Sums, record.Sums)
			}
		}
		for _, bucket := range buckets {
			if err := mergeResourceRollup(tx, bucket); err != nil {
				return err
			}
		}
		if sourceResolution == 0 {
			for _, table := range []string{"records", "records_long_term"} {
				if err := tx.Exec("DELETE FROM "+table+" WHERE time >= ? AND time < ?", windowStart, windowEnd).Error; err != nil {
					return err
				}
			}
			return nil
		}
		return tx.Where("resolution_seconds = ? AND time >= ? AND time < ?", int64(sourceResolution/time.Second), windowStart, windowEnd).
			Delete(&models.ResourceRollup{}).Error
	})
	return true, err
}

func compactGPUWindow(db *gorm.DB, sourceResolution, targetResolution time.Duration, eligibleEnd time.Time) (bool, error) {
	tables := []string{"gpu_rollups"}
	if sourceResolution == 0 {
		tables = []string{"gpu_records", "gpu_records_long_term"}
	}
	oldest, err := oldestSourceTime(db, tables, sourceResolution, eligibleEnd)
	if err != nil || oldest.IsZero() {
		return false, err
	}
	windowStart := oldest.Truncate(targetResolution)
	windowEnd := windowStart.Add(targetResolution)
	if windowEnd.After(eligibleEnd) {
		return false, nil
	}

	type gpuKey struct {
		client string
		device int
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		buckets := make(map[gpuKey]*models.GPURollup)
		if sourceResolution == 0 {
			var raw []models.GPURecord
			if err := tx.Table("gpu_records").Where("time >= ? AND time < ?", windowStart, windowEnd).Find(&raw).Error; err != nil {
				return err
			}
			var legacy []models.GPURecord
			if err := tx.Table("gpu_records_long_term").Where("time >= ? AND time < ?", windowStart, windowEnd).Find(&legacy).Error; err != nil {
				return err
			}
			rawDevices := make(map[gpuKey]struct{}, len(raw))
			for _, record := range raw {
				rawDevices[gpuKey{client: record.Client, device: record.DeviceIndex}] = struct{}{}
			}
			for _, record := range legacy {
				key := gpuKey{client: record.Client, device: record.DeviceIndex}
				if _, hasRaw := rawDevices[key]; !hasRaw {
					raw = append(raw, record)
				}
			}
			for _, record := range raw {
				key := gpuKey{client: record.Client, device: record.DeviceIndex}
				bucket := buckets[key]
				if bucket == nil {
					bucket = &models.GPURollup{
						Client: record.Client, DeviceIndex: record.DeviceIndex, DeviceName: record.DeviceName,
						Time: models.FromTime(windowStart), ResolutionSeconds: int64(targetResolution / time.Second),
						Sums: models.MetricSums{},
					}
					buckets[key] = bucket
				}
				bucket.Count++
				addGPURecordSums(bucket.Sums, record, 1)
			}
		} else {
			var source []models.GPURollup
			if err := tx.Where("resolution_seconds = ? AND time >= ? AND time < ?", int64(sourceResolution/time.Second), windowStart, windowEnd).Find(&source).Error; err != nil {
				return err
			}
			for _, record := range source {
				key := gpuKey{client: record.Client, device: record.DeviceIndex}
				bucket := buckets[key]
				if bucket == nil {
					bucket = &models.GPURollup{
						Client: record.Client, DeviceIndex: record.DeviceIndex, DeviceName: record.DeviceName,
						Time: models.FromTime(windowStart), ResolutionSeconds: int64(targetResolution / time.Second),
						Sums: models.MetricSums{},
					}
					buckets[key] = bucket
				}
				bucket.Count += record.Count
				mergeMetricSums(bucket.Sums, record.Sums)
			}
		}
		for _, bucket := range buckets {
			if err := mergeGPURollup(tx, bucket); err != nil {
				return err
			}
		}
		if sourceResolution == 0 {
			for _, table := range []string{"gpu_records", "gpu_records_long_term"} {
				if err := tx.Exec("DELETE FROM "+table+" WHERE time >= ? AND time < ?", windowStart, windowEnd).Error; err != nil {
					return err
				}
			}
			return nil
		}
		return tx.Where("resolution_seconds = ? AND time >= ? AND time < ?", int64(sourceResolution/time.Second), windowStart, windowEnd).
			Delete(&models.GPURollup{}).Error
	})
	return true, err
}

func compactPingWindow(db *gorm.DB, sourceResolution, targetResolution time.Duration, eligibleEnd time.Time) (bool, error) {
	tables := []string{"ping_rollups"}
	if sourceResolution == 0 {
		tables = []string{"ping_records"}
	}
	oldest, err := oldestSourceTime(db, tables, sourceResolution, eligibleEnd)
	if err != nil || oldest.IsZero() {
		return false, err
	}
	windowStart := oldest.Truncate(targetResolution)
	windowEnd := windowStart.Add(targetResolution)
	if windowEnd.After(eligibleEnd) {
		return false, nil
	}

	type pingKey struct {
		client string
		taskID uint
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		buckets := make(map[pingKey]*models.PingRollup)
		if sourceResolution == 0 {
			var raw []models.PingRecord
			if err := tx.Where("time >= ? AND time < ?", windowStart, windowEnd).Find(&raw).Error; err != nil {
				return err
			}
			for _, record := range raw {
				key := pingKey{client: record.Client, taskID: record.TaskId}
				bucket := buckets[key]
				if bucket == nil {
					bucket = &models.PingRollup{
						Client: record.Client, TaskID: record.TaskId, Time: models.FromTime(windowStart),
						ResolutionSeconds: int64(targetResolution / time.Second),
					}
					buckets[key] = bucket
				}
				addPingValue(bucket, record.Value)
			}
		} else {
			var source []models.PingRollup
			if err := tx.Where("resolution_seconds = ? AND time >= ? AND time < ?", int64(sourceResolution/time.Second), windowStart, windowEnd).Find(&source).Error; err != nil {
				return err
			}
			for _, record := range source {
				key := pingKey{client: record.Client, taskID: record.TaskID}
				bucket := buckets[key]
				if bucket == nil {
					bucket = &models.PingRollup{
						Client: record.Client, TaskID: record.TaskID, Time: models.FromTime(windowStart),
						ResolutionSeconds: int64(targetResolution / time.Second),
					}
					buckets[key] = bucket
				}
				mergePingRollup(bucket, &record)
			}
		}
		for _, bucket := range buckets {
			if err := mergeStoredPingRollup(tx, bucket); err != nil {
				return err
			}
		}
		if sourceResolution == 0 {
			return tx.Where("time >= ? AND time < ?", windowStart, windowEnd).Delete(&models.PingRecord{}).Error
		}
		return tx.Where("resolution_seconds = ? AND time >= ? AND time < ?", int64(sourceResolution/time.Second), windowStart, windowEnd).
			Delete(&models.PingRollup{}).Error
	})
	return true, err
}

func mergeResourceRollup(tx *gorm.DB, incoming *models.ResourceRollup) error {
	var existing models.ResourceRollup
	err := tx.Where("client = ? AND time = ? AND resolution_seconds = ?", incoming.Client, incoming.Time, incoming.ResolutionSeconds).
		First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return tx.Create(incoming).Error
	}
	if err != nil {
		return err
	}
	existing.Count += incoming.Count
	mergeMetricSums(existing.Sums, incoming.Sums)
	return tx.Save(&existing).Error
}

func mergeGPURollup(tx *gorm.DB, incoming *models.GPURollup) error {
	var existing models.GPURollup
	err := tx.Where("client = ? AND device_index = ? AND time = ? AND resolution_seconds = ?",
		incoming.Client, incoming.DeviceIndex, incoming.Time, incoming.ResolutionSeconds).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return tx.Create(incoming).Error
	}
	if err != nil {
		return err
	}
	existing.Count += incoming.Count
	mergeMetricSums(existing.Sums, incoming.Sums)
	if incoming.DeviceName != "" {
		existing.DeviceName = incoming.DeviceName
	}
	return tx.Save(&existing).Error
}

func mergeStoredPingRollup(tx *gorm.DB, incoming *models.PingRollup) error {
	var existing models.PingRollup
	err := tx.Where("client = ? AND task_id = ? AND time = ? AND resolution_seconds = ?",
		incoming.Client, incoming.TaskID, incoming.Time, incoming.ResolutionSeconds).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return tx.Create(incoming).Error
	}
	if err != nil {
		return err
	}
	mergePingRollup(&existing, incoming)
	return tx.Save(&existing).Error
}

func mergeMetricSums(target, source models.MetricSums) {
	for name, value := range source {
		target[name] += value
	}
}

func addRecordSums(sums models.MetricSums, record models.Record, weight float64) {
	sums["cpu"] += float64(record.Cpu) * weight
	sums["gpu"] += float64(record.Gpu) * weight
	sums["ram"] += float64(record.Ram) * weight
	sums["ram_total"] += float64(record.RamTotal) * weight
	sums["swap"] += float64(record.Swap) * weight
	sums["swap_total"] += float64(record.SwapTotal) * weight
	sums["load"] += float64(record.Load) * weight
	sums["temp"] += float64(record.Temp) * weight
	sums["disk"] += float64(record.Disk) * weight
	sums["disk_total"] += float64(record.DiskTotal) * weight
	sums["net_in"] += float64(record.NetIn) * weight
	sums["net_out"] += float64(record.NetOut) * weight
	sums["net_total_up"] += float64(record.NetTotalUp) * weight
	sums["net_total_down"] += float64(record.NetTotalDown) * weight
	sums["traffic_up"] += float64(record.TrafficUp) * weight
	sums["traffic_down"] += float64(record.TrafficDown) * weight
	sums["process"] += float64(record.Process) * weight
	sums["connections"] += float64(record.Connections) * weight
	sums["connections_udp"] += float64(record.ConnectionsUdp) * weight
}

func addGPURecordSums(sums models.MetricSums, record models.GPURecord, weight float64) {
	sums["mem_total"] += float64(record.MemTotal) * weight
	sums["mem_used"] += float64(record.MemUsed) * weight
	sums["utilization"] += float64(record.Utilization) * weight
	sums["temperature"] += float64(record.Temperature) * weight
}

func addPingValue(bucket *models.PingRollup, value int) {
	bucket.TotalCount++
	if value < 0 {
		bucket.LossCount++
		return
	}
	numeric := float64(value)
	validCount := bucket.TotalCount - bucket.LossCount
	if validCount == 1 || numeric < bucket.Minimum {
		bucket.Minimum = numeric
	}
	if validCount == 1 || numeric > bucket.Maximum {
		bucket.Maximum = numeric
	}
	bucket.ValueSum += numeric
}

func mergePingRollup(target, source *models.PingRollup) {
	targetValid := target.TotalCount - target.LossCount
	sourceValid := source.TotalCount - source.LossCount
	if sourceValid > 0 {
		if targetValid == 0 || source.Minimum < target.Minimum {
			target.Minimum = source.Minimum
		}
		if targetValid == 0 || source.Maximum > target.Maximum {
			target.Maximum = source.Maximum
		}
	}
	target.TotalCount += source.TotalCount
	target.LossCount += source.LossCount
	target.ValueSum += source.ValueSum
}

func deleteRollupsBefore(db *gorm.DB, table string, resolution time.Duration, cutoff time.Time) error {
	if resolution <= 0 {
		return errors.New("invalid rollup resolution")
	}
	return db.Exec(
		"DELETE FROM "+table+" WHERE rowid IN (SELECT rowid FROM "+table+" WHERE resolution_seconds = ? AND time < ? LIMIT 1000)",
		int64(resolution/time.Second), cutoff,
	).Error
}
