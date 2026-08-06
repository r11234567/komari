package dbcore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/komari-monitor/komari/cmd/flags"
	"gorm.io/gorm"
)

// SQLiteStorageSize returns the size of the database and its WAL/SHM files.
func SQLiteStorageSize() (int64, error) {
	if !flags.IsSQLite() {
		return 0, errors.New("database size is only available for SQLite")
	}

	path := strings.TrimPrefix(strings.TrimSpace(flags.DatabaseFile), "file:")
	if index := strings.IndexByte(path, '?'); index >= 0 {
		path = path[:index]
	}
	if path == "" || path == ":memory:" || strings.Contains(strings.ToLower(flags.DatabaseFile), "mode=memory") {
		return 0, errors.New("database is not backed by a local file")
	}

	var total int64
	foundDatabase := false
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		switch {
		case err == nil:
			total += info.Size()
			if suffix == "" {
				foundDatabase = true
			}
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return 0, fmt.Errorf("stat database file %q: %w", path+suffix, err)
		}
	}
	if !foundDatabase {
		return 0, fmt.Errorf("database file %q does not exist", path)
	}
	return total, nil
}

// MaintainSQLite refreshes indexes and planner statistics, then rewrites the
// database to reclaim unused pages. It is serialized with normal SQLite writes.
func MaintainSQLite(ctx context.Context) error {
	if !flags.IsSQLite() {
		return errors.New("database maintenance is only supported for SQLite")
	}

	return Write(ctx, func(db *gorm.DB) error {
		steps := []struct {
			name string
			sql  string
		}{
			{"checkpoint WAL", "PRAGMA wal_checkpoint(PASSIVE)"},
			{"reindex", "REINDEX"},
			{"analyze", "ANALYZE"},
			{"vacuum", "VACUUM"},
			{"truncate WAL", "PRAGMA wal_checkpoint(TRUNCATE)"},
		}
		for _, step := range steps {
			if err := db.WithContext(ctx).Exec(step.sql).Error; err != nil {
				return fmt.Errorf("%s: %w", step.name, err)
			}
		}
		return nil
	})
}
