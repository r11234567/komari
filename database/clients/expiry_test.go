package clients

import (
	"errors"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestExpiryWallClockValidationPreservationAndConcurrentRenewal(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.Client{}, &models.BillingPriceVersion{}, &models.BillingFXSnapshot{}, &models.BillingEntry{}))
	stamp := time.Date(2026, 10, 26, 9, 30, 45, 123456000, time.UTC)
	require.NoError(t, db.Create(&models.Client{UUID: "expiry", Token: "expiry-token", Name: "Expiry", ExpiredAt: &stamp, ExpiryTimezone: "America/New_York"}).Error)
	require.NoError(t, saveClient(db, map[string]interface{}{"uuid": "expiry", "name": "Renamed"}))
	var client models.Client
	require.NoError(t, db.First(&client, "uuid = ?", "expiry").Error)
	require.True(t, stamp.Equal(*client.ExpiredAt))
	for _, update := range []map[string]interface{}{
		{"expiry_timezone": "Not/AZone"},
		{"expiry_local_datetime": "2026-03-08T02:30:45", "expiry_timezone": "America/New_York"},
		{"expiry_local_datetime": "2026-10-26T09:30:00", "expired_at": stamp},
		{"expired_at": "2026-10-26"},
	} {
		update["uuid"] = "expiry"
		require.Error(t, saveClient(db, update))
	}
	require.NoError(t, saveClient(db, map[string]interface{}{"uuid": "expiry", "expiry_timezone": "America/New_York", "expiry_local_datetime": "2026-11-01T09:30:45"}))
	require.NoError(t, db.First(&client, "uuid = ?", "expiry").Error)
	require.Equal(t, "2026-11-01T14:30:45Z", client.ExpiredAt.UTC().Format(time.RFC3339))
	// A stale renewal must not overwrite an administrator's newer timestamp.
	err = saveClient(db, map[string]interface{}{"uuid": "expiry", "expired_at": stamp.AddDate(0, 1, 0), "_match_expired_at": stamp})
	require.True(t, errors.Is(err, ErrExpiryAlreadyChanged), "%v", err)
	require.NoError(t, saveClient(db, map[string]interface{}{"uuid": "expiry", "expired_at": nil}))
	client = models.Client{}
	require.NoError(t, db.First(&client, "uuid = ?", "expiry").Error)
	require.Nil(t, client.ExpiredAt)
}
