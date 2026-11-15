package metric

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type telemetryBackupFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type telemetryBackupManifest struct {
	Version int                   `json:"schema_version"`
	Store   string                `json:"store_id"`
	Through int64                 `json:"accepted_through"`
	Files   []telemetryBackupFile `json:"files"`
}

// BackupBundle freezes this process's archive publisher and ingress until the
// SQL snapshot and every catalogued immutable file have been copied. No cold
// file is decoded or recompressed. A partial bundle is never returned as valid.
// The application also holds its exclusive lifecycle lease, stops metadata
// delivery and takes the metadata snapshot through the supplied callback.
func (t *Telemetry) BackupBundle(ctx context.Context, databasePath, archiveDir string, metadata func() error) error {
	return t.BackupBundleAfterSnapshot(ctx, databasePath, archiveDir, metadata, nil)
}

// BackupBundleAfterSnapshot keeps ingress frozen only until both SQL snapshots
// are durable. The trusted callback may downgrade the application's lifecycle
// gate; immutable-file copying retains archive ownership and file pins. The
// catalog is read from the SQL snapshot, never from later accepted live data.
func (t *Telemetry) BackupBundleAfterSnapshot(ctx context.Context, databasePath, archiveDir string, metadata func() error, snapshotReady func()) error {
	if err := t.CheckCapacity(ctx, true); err != nil {
		return err
	}
	select {
	case t.archiveGate <- struct{}{}:
		defer func() { <-t.archiveGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return err
	}
	defer func() { release() }()
	if err = t.claimWork(ctx, "archive"); err != nil {
		return err
	}
	defer t.releaseWork("archive")
	// Pin the entire catalog before the SQL snapshot. Expiration uses the
	// same publication lease; raw deletion also respects these durable pins.
	lease := telemetryID()
	if _, err = t.s.db.ExecContext(ctx, t.bind("INSERT INTO "+t.table("leases")+" (id,segment_id,expires) SELECT ?,id,? FROM "+t.table("segments")+" WHERE state IN ('archived','deleting','complete')"), lease, time.Now().Add(10*time.Minute).UnixNano()); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		t.s.db.ExecContext(cleanup, t.bind("DELETE FROM "+t.table("leases")+" WHERE id=?"), lease)
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		tick := time.NewTicker(max(time.Millisecond, t.cfg.WorkLease/3))
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if e := t.renewWork(ctx, t.s.db, "archive"); e != nil {
					cancel()
					return
				}
				if _, e := t.s.db.ExecContext(ctx, t.bind("UPDATE "+t.table("leases")+" SET expires=? WHERE id=?"), time.Now().Add(10*time.Minute).UnixNano(), lease); e != nil {
					cancel()
					return
				}
			}
		}
	}()
	if metadata != nil {
		if err = metadata(); err != nil {
			return err
		}
	}
	if err = t.s.BackupSQLite(ctx, databasePath); err != nil {
		return err
	}
	snapshot, err := sql.Open("sqlite3", setSQLiteDSNParam(setSQLiteDSNParam(sqliteFileDSN(databasePath), "mode", "ro"), "_txlock", "deferred"))
	if err != nil {
		return err
	}
	defer snapshot.Close()
	manifest := telemetryBackupManifest{Version: 1, Store: t.instance}
	manifest.Through, err = t.progress(ctx, snapshot, "ingest")
	if err != nil {
		return err
	}
	release()
	release = func() {}
	if snapshotReady != nil {
		snapshotReady()
	}
	if err = t.fault("backup-snapshot-ready"); err != nil {
		return err
	}
	if err = os.MkdirAll(archiveDir, 0700); err != nil {
		return err
	}
	mf, err := os.OpenFile(filepath.Join(archiveDir, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer mf.Close()
	quoted, _ := json.Marshal(manifest.Store)
	if _, err = fmt.Fprintf(mf, "{\"schema_version\":1,\"store_id\":%s,\"accepted_through\":%d,\"files\":[", quoted, manifest.Through); err != nil {
		return err
	}
	encoder := json.NewEncoder(mf)
	first := true
	// Page the catalog, too: archive history must not be an in-memory slice.
	after := ""
	copied := map[string]bool{}
	for {
		rows, e := snapshot.QueryContext(ctx, t.bind("SELECT id FROM "+t.table("segments")+" WHERE id>? AND state IN ('archived','deleting','complete') ORDER BY id LIMIT 128"), after)
		if e != nil {
			return e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				break
			}
			ids = append(ids, id)
		}
		re := rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if re != nil {
			return re
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err = ctx.Err(); err != nil {
				return err
			}
			seg, e := t.segment(ctx, snapshot, id)
			if e != nil {
				return e
			}
			rel := t.segmentPath(seg)
			if copied[rel] {
				continue
			}
			if len(copied) >= 100000 {
				return ErrTelemetryBackpressure
			}
			if seg.ArchiveFile != "" {
				if e = snapshot.QueryRowContext(ctx, t.bind("SELECT COALESCE(MAX(archive_offset+file_bytes),0) FROM "+t.table("segments")+" WHERE archive_file=? AND state IN ('archived','deleting','complete')"), seg.ArchiveFile).Scan(&seg.FileBytes); e != nil {
					return e
				}
				seg.ArchiveOffset = 0
			}
			info, e := t.root.Lstat(rel)
			if e != nil {
				return e
			}
			if !telemetryArchiveExtentValid(info, seg) {
				return fmt.Errorf("backup archive type/size mismatch: %s", id)
			}
			src, e := t.root.Open(rel)
			if e != nil {
				return e
			}
			dest := filepath.Join(archiveDir, rel)
			if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
				src.Close()
				return e
			}
			out, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				src.Close()
				return e
			}
			var output io.Writer = out
			var hash string
			h := sha256.New()
			if seg.Hash != "" {
				output = io.MultiWriter(out, h)
			}
			n, e := io.Copy(output, &telemetryContextReader{ctx: ctx, r: telemetryArchiveReader(src, seg)})
			src.Close()
			if e == nil {
				e = out.Sync()
			}
			ce := out.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
			if seg.Hash != "" {
				hash = hex.EncodeToString(h.Sum(nil))
			}
			if n != seg.FileBytes || hash != seg.Hash {
				return fmt.Errorf("backup checksum mismatch: %s", id)
			}
			if !first {
				if _, err = io.WriteString(mf, ","); err != nil {
					return err
				}
			}
			first = false
			if err = encoder.Encode(telemetryBackupFile{Path: filepath.ToSlash(rel), SHA256: hash, Bytes: n}); err != nil {
				return err
			}
			copied[rel] = true
			if err = t.renewWork(ctx, t.s.db, "archive"); err != nil {
				return err
			}
		}
		after = ids[len(ids)-1]
	}
	if _, err = io.WriteString(mf, "]}\n"); err != nil {
		return err
	}
	if err = mf.Sync(); err != nil {
		return err
	}
	return mf.Close()
}

type telemetryContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *telemetryContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
