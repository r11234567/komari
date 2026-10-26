package clients

import (
	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"testing"
)

func TestTrafficClockSaveValidatesAndPreservesPartialEdits(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:traffic-clock-policy?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.Client{}))
	day := 31
	require.NoError(t, db.Create(&models.Client{UUID: "node", Token: "token", TrafficResetDay: &day}).Error)
	require.NoError(t, saveClient(db, map[string]interface{}{"uuid": "node", "traffic_reset_time": "12:45", "traffic_reset_timezone": "UTC-05:30"}))
	var saved models.Client
	require.NoError(t, db.First(&saved, "uuid = ?", "node").Error)
	require.Equal(t, "12:45:00", saved.TrafficResetTime)
	require.Equal(t, "UTC-05:30", saved.TrafficResetTimezone)
	require.NoError(t, saveClient(db, map[string]interface{}{"uuid": "node", "name": "renamed"}))
	require.NoError(t, db.First(&saved, "uuid = ?", "node").Error)
	require.Equal(t, "12:45:00", saved.TrafficResetTime)
	for _, bad := range []string{"24:00", "12:60", "12x:30", "12:30evil"} {
		require.Error(t, saveClient(db, map[string]interface{}{"uuid": "node", "traffic_reset_time": bad}))
	}
	require.Error(t, saveClient(db, map[string]interface{}{"uuid": "node", "traffic_reset_timezone": "UTC+15"}))
}
