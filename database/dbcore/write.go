package dbcore

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"gorm.io/gorm"
)

var sqliteWriteGate = make(chan struct{}, 1)
var pendingWriters atomic.Int64

// Write serializes SQLite writers and retries transient lock failures. The
// callback is not retried after it succeeds, so it must return every error.
func Write(ctx context.Context, fn func(*gorm.DB) error) error {
	if !flags.IsSQLite() {
		return fn(GetDBInstance().WithContext(ctx))
	}
	pendingWriters.Add(1)
	select {
	case sqliteWriteGate <- struct{}{}:
		pendingWriters.Add(-1)
		defer func() { <-sqliteWriteGate }()
	case <-ctx.Done():
		pendingWriters.Add(-1)
		return ctx.Err()
	}

	delays := [...]time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}
	var err error
	for _, delay := range delays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
		err = fn(GetDBInstance().WithContext(ctx))
		if err == nil || !IsBusyError(err) {
			return err
		}
	}
	return err
}

// TryMaintenance runs low-priority work only when no foreground writer is
// active or queued. A skipped maintenance pass is intentionally not an error.
func TryMaintenance(ctx context.Context, fn func(*gorm.DB) error) (bool, error) {
	if !flags.IsSQLite() {
		return true, fn(GetDBInstance().WithContext(ctx))
	}
	if pendingWriters.Load() > 0 {
		return false, nil
	}
	select {
	case sqliteWriteGate <- struct{}{}:
		defer func() { <-sqliteWriteGate }()
		if pendingWriters.Load() > 0 {
			return false, nil
		}
		return true, fn(GetDBInstance().WithContext(ctx))
	default:
		return false, nil
	}
}

func IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") || strings.Contains(text, "database table is locked") || strings.Contains(text, "sqlite_busy")
}

// WriteQueueDepth reports whether the SQLite writer is currently occupied.
func WriteQueueDepth() int { return len(sqliteWriteGate) + int(pendingWriters.Load()) }
