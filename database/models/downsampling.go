package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// MetricSums stores weighted metric sums for a downsampled bucket.
type MetricSums map[string]float64

func (s *MetricSums) Scan(value interface{}) error {
	var raw []byte
	switch typed := value.(type) {
	case nil:
		*s = MetricSums{}
		return nil
	case []byte:
		raw = typed
	case string:
		raw = []byte(typed)
	default:
		return fmt.Errorf("failed to scan MetricSums: unsupported value type %T", value)
	}
	if len(raw) == 0 {
		*s = MetricSums{}
		return nil
	}
	return json.Unmarshal(raw, s)
}

func (s MetricSums) Value() (driver.Value, error) {
	if s == nil {
		s = MetricSums{}
	}
	return json.Marshal(s)
}

// ResourceRollup is one persisted resource-metric bucket.
type ResourceRollup struct {
	Client            string     `gorm:"type:varchar(36);primaryKey;index:idx_resource_rollup_lookup,priority:1"`
	Time              LocalTime  `gorm:"primaryKey;index:idx_resource_rollup_lookup,priority:3"`
	ResolutionSeconds int64      `gorm:"primaryKey;index:idx_resource_rollup_lookup,priority:2"`
	Count             int64      `gorm:"not null"`
	Sums              MetricSums `gorm:"type:text;not null"`
}

// GPURollup is one persisted per-device GPU bucket.
type GPURollup struct {
	Client            string     `gorm:"type:varchar(36);primaryKey;index:idx_gpu_rollup_lookup,priority:1"`
	DeviceIndex       int        `gorm:"primaryKey"`
	DeviceName        string     `gorm:"type:varchar(100)"`
	Time              LocalTime  `gorm:"primaryKey;index:idx_gpu_rollup_lookup,priority:3"`
	ResolutionSeconds int64      `gorm:"primaryKey;index:idx_gpu_rollup_lookup,priority:2"`
	Count             int64      `gorm:"not null"`
	Sums              MetricSums `gorm:"type:text;not null"`
}

// PingRollup preserves loss, average, minimum and maximum values for a bucket.
type PingRollup struct {
	Client            string    `gorm:"type:varchar(36);primaryKey;index:idx_ping_rollup_lookup,priority:1"`
	TaskID            uint      `gorm:"primaryKey;index:idx_ping_rollup_task_lookup,priority:1"`
	Time              LocalTime `gorm:"primaryKey;index:idx_ping_rollup_lookup,priority:3;index:idx_ping_rollup_task_lookup,priority:3"`
	ResolutionSeconds int64     `gorm:"primaryKey;index:idx_ping_rollup_lookup,priority:2;index:idx_ping_rollup_task_lookup,priority:2"`
	TotalCount        int64     `gorm:"not null"`
	LossCount         int64     `gorm:"not null"`
	ValueSum          float64   `gorm:"not null"`
	Minimum           float64   `gorm:"not null"`
	Maximum           float64   `gorm:"not null"`
}
