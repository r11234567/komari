package trafficledger

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/gorm"
)

// Mark the already-started transaction for GORM's nested savepoint handling.
// The outer snapshot alone owns commit/rollback on its pinned connection.
type telemetrySnapshotConn struct{ gorm.ConnPool }

func (*telemetrySnapshotConn) Commit() error {
	return errors.New("snapshot commit is owned by its outer reader")
}
func (*telemetrySnapshotConn) Rollback() error {
	return errors.New("snapshot rollback is owned by its outer reader")
}

// WithTelemetryReadSnapshot pins every accounting SELECT to one read-only view.
// Production metadata uses SQLite with _txlock=immediate for write safety.
// go-sqlite3 ignores ReadOnly TxOptions and would start BEGIN IMMEDIATE even
// for a read. Explicit BEGIN DEFERRED on a pinned connection avoids that write
// lock; query_only enforces read-only access. It is reset before pool reuse.
func WithTelemetryReadSnapshot(ctx context.Context, db *gorm.DB, read func(*gorm.DB) error) error {
	if db == nil || read == nil {
		return metric.ErrInvalidArgument
	}
	if db.Dialector.Name() != "sqlite" {
		return db.WithContext(ctx).Transaction(read, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	}
	if _, nested := db.Statement.ConnPool.(*telemetrySnapshotConn); nested {
		return db.WithContext(ctx).Transaction(read)
	}
	return db.WithContext(ctx).Connection(func(connDB *gorm.DB) (result error) {
		pool := connDB.Statement.ConnPool
		begun := false
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			var reset error
			if begun {
				_, reset = pool.ExecContext(cleanup, "ROLLBACK")
			}
			_, queryReset := pool.ExecContext(cleanup, "PRAGMA query_only=OFF")
			reset = errors.Join(reset, queryReset)
			if reset != nil { // Never return a poisoned transaction/connection to the pool.
				if conn, ok := pool.(*sql.Conn); ok {
					_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				}
				result = errors.Join(result, reset)
			}
		}()
		if _, err := pool.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
			return err
		}
		if _, err := pool.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
			return err
		}
		begun = true
		connDB.Statement.ConnPool = &telemetrySnapshotConn{pool}
		if err := read(connDB.Session(&gorm.Session{NewDB: true})); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := pool.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		begun = false
		return nil
	})
}
