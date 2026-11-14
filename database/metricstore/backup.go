package metricstore

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/komari-monitor/komari/pkg/metric"
)

// BackupSQLite keeps the active Store stable while producing a consistent
// point-in-time snapshot of all raw, rollup, digest, and migration data.
func BackupSQLite(ctx context.Context, destination string) error {
	if err := storeOperations.AcquireShared(ctx); err != nil {
		return fmt.Errorf("wait for metric store before backup: %w", err)
	}
	defer storeOperations.ReleaseShared()

	storeMu.RLock()
	defer storeMu.RUnlock()
	activeStore := store
	if activeStore == nil {
		return fmt.Errorf("metric store not initialized")
	}
	if activeStore.Driver() != metric.DriverSQLite {
		return fmt.Errorf("complete historical backup is only available for the SQLite metric store")
	}
	return activeStore.BackupSQLite(ctx, destination)
}

// BackupFull snapshots both databases and immutable history behind the same
// lifecycle barrier. A metadata outbox watermark and the SQL snapshot cannot
// straddle an ingestion acknowledged after the main database snapshot.
func BackupFull(ctx context.Context, directory string, metadata func() error) error {
	if err := storeOperations.Acquire(ctx); err != nil {
		return err
	}
	shared := false
	defer func() {
		if shared {
			storeOperations.ReleaseShared()
		} else {
			storeOperations.Release()
		}
	}()
	s := GetStore()
	if s == nil {
		return ErrStoreNotInitialized
	}
	if s.Driver() != metric.DriverSQLite {
		return fmt.Errorf("full portable history backup requires SQLite")
	}
	if t := s.Telemetry(); t != nil {
		if c := activeTelemetryConsumer.Load(); c != nil && c.apply != nil {
			for {
				n, err := t.DeliverTraffic(ctx, 256, c.apply)
				if err != nil {
					return err
				}
				if n < 256 {
					break
				}
			}
		}
		return t.BackupBundleAfterSnapshot(ctx, filepath.Join(directory, "metrics.db"), filepath.Join(directory, "telemetry"), metadata, func() {
			storeOperations.Downgrade()
			shared = true
		})
	}
	if err := metadata(); err != nil {
		return err
	}
	return s.BackupSQLite(ctx, filepath.Join(directory, "metrics.db"))
}
