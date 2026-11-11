package jsonrpc

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/pkg/metric"
	dashboardapp "github.com/komari-monitor/komari/web/dashboard"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDashboardTrafficFailuresAreIndependent(t *testing.T) {
	for _, fail := range []string{"trend", "today", "none"} {
		t.Run(fail, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "meta.db")+"?_journal_mode=WAL&_txlock=immediate"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			pool, err := db.DB()
			require.NoError(t, err)
			defer pool.Close()
			require.NoError(t, db.AutoMigrate(&models.Client{}, &models.TrafficDailyLedger{}, &models.TrafficCalibrationAdjustment{}, &models.TrafficCycleFirstDay{}))
			require.NoError(t, db.AutoMigrate(trafficledger.TelemetryModels()...))
			now := time.Date(2026, 11, 11, 5, 25, 0, 0, time.UTC)
			today := trafficledger.BeijingDay(now)
			client := models.Client{UUID: "node", Token: "fixture", TrafficLimitType: "sum", Price: 1}
			require.NoError(t, db.Create(&client).Error)
			for i, at := range []time.Time{today.Add(-time.Minute), today.Add(time.Minute)} {
				require.NoError(t, trafficledger.ApplyTelemetry(t.Context(), db, metric.TelemetryTrafficEvent{StoreID: "isolation", Source: client.UUID, IngestID: int64(i + 1), Observed: at, Received: at, Traffic: metric.TelemetryTraffic{HasDelta: true, DeltaUp: int64(i+1) * 10, DeltaDown: int64(i+1) * 20}}))
			}
			require.NoError(t, db.Create(&models.TrafficCalibrationAdjustment{Client: "node", Cycle: "2026-11-01T00:00:00Z", Day: today.AddDate(0, 0, -1).Format(time.DateOnly), UpDelta: -1, DownDelta: -2, CalibrationID: "fixture-adjustment", CreatedAt: now}).Error)
			if fail == "today" {
				require.NoError(t, db.Callback().Query().After("gorm:query").Register("fail-today", func(tx *gorm.DB) {
					if tx.Statement.Table == "traffic_daily_ledgers" && strings.Contains(tx.Statement.SQL.String(), "day = ") {
						tx.AddError(errors.New("injected today's query failure"))
					}
				}))
			}
			load := func(context.Context, []string, time.Time, time.Duration, time.Duration) (*dashboardapp.TrafficTrendResult, error) {
				if fail == "trend" {
					return nil, errors.New("injected trend failure")
				}
				return &dashboardapp.TrafficTrendResult{Coverage: "complete", Buckets: []dashboardapp.TrafficTrendBucket{{Start: today, Up: 20, Down: 40}}}, nil
			}
			result, err := loadDashboardTrafficFrom(t.Context(), db, []models.Client{client}, now, 5, true, load)
			require.NoError(t, err)
			require.Empty(t, result.Error)
			require.Len(t, result.Daily, 30)
			yesterday := result.Daily[len(result.Daily)-2]
			require.Equal(t, int64(9), yesterday.Up)
			require.Equal(t, int64(18), yesterday.Down)
			require.Equal(t, int64(27), yesterday.Billable)
			if fail == "trend" {
				require.NotEmpty(t, result.TrendError)
				require.Empty(t, result.TodayError)
				require.Equal(t, int64(60), result.TodayBillable)
				require.Len(t, result.Ranking, 1)
			}
			if fail == "today" {
				require.NotEmpty(t, result.TodayError)
				require.NotEmpty(t, result.RankingError)
				require.Nil(t, result.Ranking)
				require.NotNil(t, result.Daily[29].Available)
				require.False(t, *result.Daily[29].Available)
				require.Empty(t, result.TrendError)
				require.Len(t, result.Hourly, 1)
			}
			if fail == "none" {
				require.Empty(t, result.TodayError)
				require.Empty(t, result.TrendError)
				require.NotNil(t, result.AccountingThrough)
				require.Equal(t, int64(60), result.TodayBillable)
			}
		})
	}
}
