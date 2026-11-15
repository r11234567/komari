package metric

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

type telemetryCapacity struct {
	sync.Mutex
	checked time.Time
	free    uint64
	err     error
	paths   []string
}

func (t *Telemetry) initializeCapacity(ctx context.Context) error {
	t.capacity.paths = []string{t.cfg.ArchiveDirectory}
	if t.s.cfg.Driver == DriverSQLite {
		rows, err := t.s.db.QueryContext(ctx, "PRAGMA database_list")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var seq int
			var name, path string
			if err = rows.Scan(&seq, &name, &path); err != nil {
				return err
			}
			if name == "main" && path != "" {
				t.capacity.paths = append(t.capacity.paths, filepath.Dir(path))
			}
		}
		return rows.Err()
	}
	return nil
}

// CheckCapacity reserves disk for ingestion/accounting. Export and backup use
// a larger floor. The filesystem probe is cached briefly, never a SQL COUNT or
// recursive directory walk. A disk-full SQL commit still fails without an ACK.
func (t *Telemetry) CheckCapacity(ctx context.Context, lowPriority bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.capacity.Lock()
	defer t.capacity.Unlock()
	if time.Since(t.capacity.checked) > 5*time.Second {
		t.capacity.free = ^uint64(0)
		t.capacity.err = nil
		for _, path := range t.capacity.paths {
			free, err := telemetryFreeBytes(path)
			if err != nil {
				t.capacity.err = err
				break
			}
			t.capacity.free = min(t.capacity.free, free)
		}
		t.capacity.checked = time.Now()
	}
	reserve := uint64(64 << 20)
	if lowPriority {
		reserve = 256 << 20
	}
	if t.capacity.err != nil {
		return fmt.Errorf("%w: cannot inspect telemetry disk reserve: %v", ErrTelemetryBackpressure, t.capacity.err)
	}
	if t.capacity.free < reserve {
		return fmt.Errorf("%w: telemetry disk reserve is low", ErrTelemetryBackpressure)
	}
	return nil
}
