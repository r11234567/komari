package metricstore

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/metric"
)

var telemetryRuntime atomic.Bool
var activeTelemetrySettings atomic.Pointer[MetricStoreConfig]

// SQL and archive identity move together in an offline operation. Reloading
// into an empty schema/different archive root would silently hide history.
func ValidateTelemetrySettings(cfg *MetricStoreConfig) error {
	if _, err := telemetryConfig(cfg); err != nil {
		return err
	}
	if old := activeTelemetrySettings.Load(); telemetryRuntime.Load() && old != nil {
		if targetFingerprint(old) != targetFingerprint(cfg) || old.TablePrefix != cfg.TablePrefix || filepath.Clean(old.ArchiveDirectory) != filepath.Clean(cfg.ArchiveDirectory) {
			return fmt.Errorf("telemetry database, table prefix and archive directory require a stopped-service migration; live retention/capacity updates remain available")
		}
	}
	return nil
}

type telemetryConsumer struct {
	apply func(context.Context, metric.TelemetryTrafficEvent) error
}

var activeTelemetryConsumer atomic.Pointer[telemetryConsumer]

type telemetryLedgerGC struct {
	apply func(context.Context, time.Time, int) (int, error)
}

var activeTelemetryLedgerGC atomic.Pointer[telemetryLedgerGC]

func ConfigureTelemetryLedgerGC(apply func(context.Context, time.Time, int) (int, error)) {
	activeTelemetryLedgerGC.Store(&telemetryLedgerGC{apply: apply})
}

func ConfigureTelemetryConsumer(apply func(context.Context, metric.TelemetryTrafficEvent) error) {
	activeTelemetryConsumer.Store(&telemetryConsumer{apply: apply})
}

func telemetryConfig(cfg *MetricStoreConfig) (metric.TelemetryConfig, error) {
	dir := cfg.ArchiveDirectory
	if dir == "" {
		dir = "./data/telemetry"
	}
	hot, segment, budget := cfg.HotWindowMinutes, cfg.ArchiveSegmentMinutes, cfg.HotBudgetMiB
	if hot == 0 {
		hot = 15
	}
	if segment == 0 || segment == 15 {
		// Retire the old quarter-hour file cadence. Shared hot capacity, per-
		// segment bytes and observation-day rollover still trigger handoff.
		segment = 1440
	}
	if budget == 0 {
		budget = 16
	} else if budget == 1024 {
		// Replace the retired one-GiB default, rather than carrying it into a
		// store whose hot table is only a bounded handoff queue.
		budget = 16
	}
	// HotRetentionMinutes is a retired setting. Raw SQL rows are only a durable
	// handoff queue; a saved one-day setting must not keep duplicating archives.
	if hot < 1 || hot > 120 || segment < 1 || segment > 1440 || budget < 4 || budget > 65536 || cfg.ArchiveRetentionDays < 0 || cfg.ArchiveRetentionDays > 3650 {
		return metric.TelemetryConfig{}, fmt.Errorf("invalid telemetry retention/capacity settings")
	}
	return metric.TelemetryConfig{ArchiveDirectory: dir, RawWindow: time.Duration(hot) * time.Minute, SegmentDuration: time.Duration(segment) * time.Minute, MaxHotBytes: int64(budget) << 20}, nil
}

func enableTelemetryStore(ctx context.Context, s *metric.Store, cfg *MetricStoreConfig) error {
	c, err := telemetryConfig(cfg)
	if err != nil {
		return err
	}
	if err = s.EnableTelemetry(ctx, c); err != nil {
		return err
	}
	return nil
}

func MigrateTelemetryOffline(ctx context.Context, s *metric.Store, cfg *MetricStoreConfig, observe func(string, int64)) error {
	c, err := telemetryConfig(cfg)
	if err != nil {
		return err
	}
	return s.MigrateLegacy(ctx, c, observe)
}

func PrepareTelemetryMigration(ctx context.Context) (metric.LegacyMigrationState, error) {
	cfg, err := config.GetManyAs[MetricStoreConfig]()
	if err != nil {
		return metric.LegacyMigrationState{}, err
	}
	c, err := telemetryConfig(cfg)
	if err != nil {
		return metric.LegacyMigrationState{}, err
	}
	s := GetStore()
	if s == nil {
		return metric.LegacyMigrationState{}, ErrStoreNotInitialized
	}
	return s.PrepareLegacyMigration(ctx, c)
}

func CompleteTelemetryLedgerMigration(ctx context.Context) error {
	cfg, err := config.GetManyAs[MetricStoreConfig]()
	if err != nil {
		return err
	}
	c, err := telemetryConfig(cfg)
	if err != nil {
		return err
	}
	s := GetStore()
	if s == nil {
		return ErrStoreNotInitialized
	}
	return s.CompleteLegacyLedgerMigration(ctx, c)
}

func SeedTelemetryTrafficBaseline(ctx context.Context, source string, at time.Time, up, down int64) error {
	cfg, err := config.GetManyAs[MetricStoreConfig]()
	if err != nil {
		return err
	}
	c, err := telemetryConfig(cfg)
	if err != nil {
		return err
	}
	t, err := GetStore().OpenTelemetry(ctx, c)
	if err != nil {
		return err
	}
	defer t.Close()
	return t.SeedTrafficBaseline(ctx, source, at, up, down)
}

// LegacyTrafficBaseline is used only by the stopped-service migration. It
// preserves the actual sample time, not the chart bucket's rounded timestamp.
func LegacyTrafficBaseline(ctx context.Context, source string, before time.Time) (time.Time, int64, int64, bool, error) {
	s := GetStore()
	if s == nil {
		return time.Time{}, 0, 0, false, ErrStoreNotInitialized
	}
	up, at, hasUp, err := latestReportCounter(ctx, s, MetricNetTotalUp, source, before)
	if err != nil {
		return time.Time{}, 0, 0, false, err
	}
	down, bt, hasDown, err := latestReportCounter(ctx, s, MetricNetTotalDown, source, before)
	if err != nil {
		return time.Time{}, 0, 0, false, err
	}
	if !hasUp || !hasDown {
		return time.Time{}, 0, 0, false, nil
	}
	if !at.Equal(bt) {
		return time.Time{}, 0, 0, false, fmt.Errorf("legacy traffic counters have different sample times for %s", source)
	}
	return at, up, down, true, nil
}

// ActivateTelemetry runs after legacy import/identity migrations, before any
// public listener/background writer is started. Existing sources are preserved
// and must be migrated explicitly instead of silently hiding their history.
func ActivateTelemetry(ctx context.Context) error {
	if err := storeOperations.Acquire(ctx); err != nil {
		return err
	}
	defer storeOperations.Release()
	cfg, err := config.GetManyAs[MetricStoreConfig]()
	if err != nil {
		return err
	}
	s := GetStore()
	if s == nil {
		return ErrStoreNotInitialized
	}
	if err := s.RequireTelemetryMigration(ctx); err != nil {
		return err
	}
	if err := enableTelemetryStore(ctx, s, cfg); err != nil {
		return err
	}
	telemetryRuntime.Store(true)
	copy := *cfg
	activeTelemetrySettings.Store(&copy)
	return nil
}
