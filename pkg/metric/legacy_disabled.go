//go:build !metric_legacy_fixture

package metric

import (
	"context"
	"time"
)

// Compatibility calls never start the retired engine. Telemetry maintenance
// advances through the durable due-work scheduler instead.
func (s *Store) Compact(ctx context.Context, _ time.Time) (int, error) {
	if err := s.ensureOpen(); err != nil {
		return 0, err
	}
	return 0, ctx.Err()
}
func (s *Store) CompactMetric(ctx context.Context, _ string, now time.Time) (int, error) {
	return s.Compact(ctx, now)
}
func (s *Store) CompactMetricStep(ctx context.Context, name string, now time.Time) (int, error) {
	return s.CompactMetric(ctx, name, now)
}
func (s *Store) migrateSQLiteStorageV4(ctx context.Context) error {
	return s.openRetiredSQLiteStorage(ctx)
}
func retireLegacyConfig(cfg *Config) {
	cfg.RetireLegacyStorage = true
}
