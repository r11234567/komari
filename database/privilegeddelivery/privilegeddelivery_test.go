package privilegeddelivery

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSaveWithSingleConnectionPoolDoesNotDeadlock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test_single_conn.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	// Strictly configure pool to exactly 1 open connection, reproducing SQLite production setup
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()

	require.NoError(t, db.AutoMigrate(
		&models.Client{},
		&models.ClientDeploymentProfile{},
		&models.ClientPrivilegedRevision{},
		&models.ClientPrivilegedState{}, &models.ClientHostOperation{}, &models.ClientPrivilegedReceipt{},
	))

	clientUUID := "test-agent-deadlock-prevention"
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Client{
		UUID:      clientUUID,
		Name:      "Test Agent",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)

	require.NoError(t, db.Create(&models.ClientDeploymentProfile{
		Client:    clientUUID,
		Config:    `{"platform":"linux","rescue_enabled":true,"remote_control_enabled":false}`,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)

	done := make(chan struct{})
	var rev Revision
	var saveErr error

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		defer close(done)
		rev, saveErr = SaveWithDB(db, clientUUID, Settings{
			RescueHelperEnabled: true,
			ExecutionEnabled:    false,
		}, 0, "test rescue helper activation")
	}()

	select {
	case <-ctx.Done():
		t.Fatal("SaveWithDB timed out due to database connection pool deadlock!")
	case <-done:
		require.NoError(t, saveErr)
		require.Equal(t, uint64(1), rev.Revision)
		require.True(t, rev.Settings.RescueHelperEnabled)
		require.Equal(t, StateNeedsConfirmation, rev.State)
	}
}
