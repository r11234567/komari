// Package trafficevidence stores analyzer output independently of billing and backups.
package trafficevidence

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	HourRetention     = 7 * 24 * time.Hour
	IncidentRetention = 72 * time.Hour
	Hour              = "hour"
	Incident          = "incident"
)

var active atomic.Pointer[Store]

type Store struct {
	db *sql.DB
}

func Current() *Store { return active.Load() }

func Start(path string) (func(context.Context) error, error) {
	store, err := Open(path)
	if err != nil {
		return nil, err
	}
	if !active.CompareAndSwap(nil, store) {
		store.Close()
		return nil, errors.New("traffic evidence store already open")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_ = store.Prune(ctx, now)
			}
		}
	}()
	return func(ctx context.Context) error {
		active.CompareAndSwap(store, nil)
		cancel()
		select {
		case <-done:
			return store.Close()
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil
}

func Open(path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() +
		"?mode=rw&_busy_timeout=2000&_secure_delete=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS evidence (
			agent_id TEXT NOT NULL, kind TEXT NOT NULL, record_id TEXT NOT NULL,
			revision INTEGER NOT NULL, expires_at INTEGER NOT NULL,
			summary BLOB NOT NULL, detail BLOB, finalized INTEGER NOT NULL,
			PRIMARY KEY (agent_id, kind, record_id)
		);
		CREATE INDEX IF NOT EXISTS evidence_by_agent ON evidence (agent_id, kind, revision DESC, record_id);
		CREATE INDEX IF NOT EXISTS evidence_expiry ON evidence (expires_at);
	`); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.Prune(context.Background(), time.Now()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Put(ctx context.Context, agent, kind, id string, revision, expires time.Time, summary, detail []byte, finalized bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO evidence (agent_id, kind, record_id, revision, expires_at, summary, detail, finalized)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, kind, record_id) DO UPDATE SET
			revision=excluded.revision, expires_at=excluded.expires_at, summary=excluded.summary,
			detail=excluded.detail, finalized=excluded.finalized
		WHERE evidence.finalized=0 AND evidence.revision < excluded.revision
	`, agent, kind, id, revision.UnixNano(), expires.UnixNano(), summary, detail, finalized); err != nil {
		return err
	}
	limit := 168
	if kind == Incident {
		limit = 288
	}
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM evidence WHERE agent_id=? AND kind=? AND record_id IN (
			SELECT record_id FROM evidence WHERE agent_id=? AND kind=?
			ORDER BY revision DESC, record_id LIMIT -1 OFFSET ?
		)
	`, agent, kind, agent, kind, limit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) List(ctx context.Context, agent, kind string, now time.Time) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT summary FROM evidence WHERE agent_id=? AND kind=? AND expires_at>?
		ORDER BY revision DESC, record_id
	`, agent, kind, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		records = append(records, raw)
	}
	return records, rows.Err()
}

func (s *Store) Detail(ctx context.Context, agent, id string, now time.Time) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT detail FROM evidence WHERE agent_id=? AND kind='incident' AND record_id=? AND expires_at>?
	`, agent, id, now.UnixNano()).Scan(&raw)
	return raw, err
}

func (s *Store) Prune(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM evidence WHERE expires_at <= ?", now.UnixNano())
	return err
}
