package clients

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestTrafficLimitRejectsPollutionWithoutPartialUpdate(t *testing.T) {
	for _, value := range []any{"alert(document.cookie),1gb", "1000", nil, true, []any{1}, map[string]any{"value": 1}, -1, int64(-1), 1.5, math.NaN(), math.Inf(1), math.Exp2(63), uint64(math.MaxUint64), json.Number("9223372036854775808")} {
		t.Run(fmt.Sprintf("%T/%v", value, value), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "clients.db")), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			require.NoError(t, db.AutoMigrate(&models.Client{}))
			require.NoError(t, db.Create(&models.Client{UUID: "quota-node", Name: "before", TrafficLimit: 123, TrafficLimitType: "sum"}).Error)
			require.Error(t, saveClient(db, map[string]any{"uuid": "quota-node", "name": "after", "traffic_limit": value}))
			var list []models.Client
			require.NoError(t, db.Find(&list).Error)
			require.Len(t, list, 1)
			require.Equal(t, int64(123), list[0].TrafficLimit)
			require.Equal(t, "before", list[0].Name)
		})
	}
}

func TestTrafficLimitNormalizesSupportedNumericCallers(t *testing.T) {
	for _, value := range []any{0, int32(1000), int64(1000), uint64(1000), float64(1000), json.Number("1000")} {
		t.Run(fmt.Sprintf("%T/%v", value, value), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "clients.db")), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			require.NoError(t, db.AutoMigrate(&models.Client{}))
			require.NoError(t, db.Create(&models.Client{UUID: "quota-node", TrafficLimitType: "sum"}).Error)
			updates := map[string]any{"uuid": "quota-node", "traffic_limit": value}
			require.NoError(t, saveClient(db, updates))
			require.IsType(t, int64(0), updates["traffic_limit"])
		})
	}
}
