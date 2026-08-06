package records

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newDownsamplingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&models.Record{},
		&models.GPURecord{},
		&models.PingRecord{},
		&models.ResourceRollup{},
		&models.GPURollup{},
		&models.PingRollup{},
	))
	require.NoError(t, db.Table("records_long_term").AutoMigrate(&models.Record{}))
	require.NoError(t, db.Table("gpu_records_long_term").AutoMigrate(&models.GPURecord{}))
	return db
}

func TestDownsamplingCompactsResourceGPUAndPing(t *testing.T) {
	db := newDownsamplingTestDB(t)
	window := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	client := "node-1"

	require.NoError(t, db.Create(&[]models.Record{
		{Client: client, Time: models.FromTime(window.Add(10 * time.Second)), Cpu: 10, Ram: 100},
		{Client: client, Time: models.FromTime(window.Add(40 * time.Second)), Cpu: 30, Ram: 300},
	}).Error)
	require.NoError(t, db.Create(&[]models.GPURecord{
		{Client: client, DeviceIndex: 0, DeviceName: "GPU", Time: models.FromTime(window.Add(10 * time.Second)), Utilization: 20},
		{Client: client, DeviceIndex: 0, DeviceName: "GPU", Time: models.FromTime(window.Add(40 * time.Second)), Utilization: 40},
	}).Error)
	require.NoError(t, db.Create(&[]models.PingRecord{
		{Client: client, TaskId: 1, Time: models.FromTime(window.Add(10 * time.Second)), Value: 10},
		{Client: client, TaskId: 1, Time: models.FromTime(window.Add(20 * time.Second)), Value: -1},
		{Client: client, TaskId: 1, Time: models.FromTime(window.Add(40 * time.Second)), Value: 30},
	}).Error)

	eligibleEnd := window.Add(time.Minute)
	progressed, err := compactResourceWindow(db, 0, time.Minute, eligibleEnd)
	require.True(t, progressed)
	require.NoError(t, err)
	progressed, err = compactGPUWindow(db, 0, time.Minute, eligibleEnd)
	require.True(t, progressed)
	require.NoError(t, err)
	progressed, err = compactPingWindow(db, 0, time.Minute, eligibleEnd)
	require.True(t, progressed)
	require.NoError(t, err)

	var resource models.ResourceRollup
	require.NoError(t, db.First(&resource).Error)
	require.Equal(t, int64(2), resource.Count)
	require.Equal(t, float64(40), resource.Sums["cpu"])
	require.Equal(t, float64(400), resource.Sums["ram"])

	var gpu models.GPURollup
	require.NoError(t, db.First(&gpu).Error)
	require.Equal(t, int64(2), gpu.Count)
	require.Equal(t, float64(60), gpu.Sums["utilization"])

	var ping models.PingRollup
	require.NoError(t, db.First(&ping).Error)
	require.Equal(t, int64(3), ping.TotalCount)
	require.Equal(t, int64(1), ping.LossCount)
	require.Equal(t, float64(40), ping.ValueSum)
	require.Equal(t, float64(10), ping.Minimum)
	require.Equal(t, float64(30), ping.Maximum)

	for _, model := range []interface{}{&models.Record{}, &models.GPURecord{}, &models.PingRecord{}} {
		var count int64
		require.NoError(t, db.Model(model).Count(&count).Error)
		require.Zero(t, count)
	}
}

func TestDownsamplingUsesWeightedSumsAcrossTiers(t *testing.T) {
	db := newDownsamplingTestDB(t)
	window := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	require.NoError(t, db.Create(&[]models.ResourceRollup{
		{Client: "node-1", Time: models.FromTime(window), ResolutionSeconds: 60, Count: 2, Sums: models.MetricSums{"cpu": 40}},
		{Client: "node-1", Time: models.FromTime(window.Add(time.Minute)), ResolutionSeconds: 60, Count: 1, Sums: models.MetricSums{"cpu": 100}},
	}).Error)

	progressed, err := compactResourceWindow(db, time.Minute, 5*time.Minute, window.Add(5*time.Minute))
	require.True(t, progressed)
	require.NoError(t, err)

	var rollup models.ResourceRollup
	require.NoError(t, db.Where("resolution_seconds = ?", 300).First(&rollup).Error)
	require.Equal(t, int64(3), rollup.Count)
	require.Equal(t, float64(140), rollup.Sums["cpu"])
}
