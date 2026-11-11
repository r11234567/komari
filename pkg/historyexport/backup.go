package historyexport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type backupFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type queueBackup struct {
	Version int          `json:"version"`
	Files   []backupFile `json:"files"`
}

func snapshotSQL(ctx context.Context, db *sql.DB, destination string) error {
	_, err := db.ExecContext(ctx, "VACUUM INTO ?", destination)
	return err
}
func snapshotSQLFile(ctx context.Context, source, destination string) error {
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(source)+"?mode=ro&_busy_timeout=3000&_txlock=deferred")
	if err != nil {
		return err
	}
	defer db.Close()
	return snapshotSQL(ctx, db, destination)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func hashFile(ctx context.Context, p string) (backupFile, error) {
	var result backupFile
	info, err := os.Lstat(p)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("backup contains a non-regular file")
	}
	f, err := os.Open(p)
	if err != nil {
		return result, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, contextReader{ctx, f}, make([]byte, 64<<10))
	result.Bytes, result.SHA256 = n, hex.EncodeToString(h.Sum(nil))
	return result, err
}
func copyBackupFile(ctx context.Context, source, destination string, limit int64) (backupFile, error) {
	var result backupFile
	info, err := os.Lstat(source)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("export backup source is not a regular file")
	}
	if limit < 0 {
		limit = info.Size()
	}
	if info.Size() < limit {
		return result, errors.New("export checkpoint is truncated")
	}
	in, err := os.Open(source)
	if err != nil {
		return result, err
	}
	defer in.Close()
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return result, err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(out, h), io.LimitReader(contextReader{ctx, in}, limit), make([]byte, 64<<10))
	if err == nil && n != limit {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = out.Sync()
	}
	ce := out.Close()
	if err == nil {
		err = ce
	}
	result.Bytes, result.SHA256 = n, hex.EncodeToString(h.Sum(nil))
	return result, err
}

// Backup freezes file mutations across the actual worker process and snapshots
// WAL-backed SQL files. It copies bytes, never decodes/reformats CSV input.
// The caller holds the lock until its telemetry/metadata snapshots are fixed,
// so a task cannot refer to source history newer than the restored databases.
func (q *Queue) BackupLocked(ctx context.Context, destination string) error {
	if err := q.checkGeneration(); err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	if err := snapshotSQL(ctx, q.db, filepath.Join(destination, "queue.db")); err != nil {
		return err
	}
	manifest := queueBackup{Version: 1}
	add := func(relative string, file backupFile) error {
		if len(manifest.Files) >= 65536 {
			return errors.New("export backup file budget exceeded")
		}
		file.Path = filepath.ToSlash(relative)
		manifest.Files = append(manifest.Files, file)
		return nil
	}
	file, err := hashFile(ctx, filepath.Join(destination, "queue.db"))
	if err != nil {
		return err
	}
	if err = add("queue.db", file); err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(destination, "queue.db"))+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	cursor := ""
	for {
		rows, err := db.QueryContext(ctx, "SELECT id,body FROM jobs WHERE id>? ORDER BY id LIMIT 64", cursor)
		if err != nil {
			return err
		}
		var jobs []*Job
		for rows.Next() {
			var id string
			var body []byte
			if err = rows.Scan(&id, &body); err != nil {
				break
			}
			cursor = id
			var j *Job
			j, err = decodeJob(body)
			if err != nil {
				break
			}
			if !ValidID(id) || id != j.ID {
				err = errors.New("invalid backup task identity")
				break
			}
			jobs = append(jobs, j)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Status == "expired" || j.Status == "cancelled" {
				continue
			}
			copyOne := func(relative string, limit int64, required bool) error {
				f, e := copyBackupFile(ctx, filepath.Join(q.Dir, relative), filepath.Join(destination, relative), limit)
				if !required && errors.Is(e, os.ErrNotExist) {
					return nil
				}
				if e != nil {
					return e
				}
				if j.Status == "done" && relative == j.ID+".csv" && (f.Bytes != j.OutputBytes || f.SHA256 != j.SHA256) {
					return errors.New("ready export failed backup integrity verification")
				}
				return add(relative, f)
			}
			if j.Status == "done" {
				if err = copyOne(j.ID+".csv", -1, true); err != nil {
					return err
				}
				continue
			}
			if j.Committed.Offset > 0 {
				relative := j.ID + ".csv.part"
				if _, err = os.Stat(q.Path(j.ID, ".csv")); err == nil {
					relative = j.ID + ".csv"
				}
				if err = copyOne(relative, j.Committed.Offset, j.Status != "failed"); err != nil {
					return err
				}
			}
			snap := q.Path(j.ID, ".snapshot")
			if _, err = os.Stat(snap); err == nil {
				err = filepath.WalkDir(snap, func(p string, d fs.DirEntry, e error) error {
					if e != nil {
						return e
					}
					if d.IsDir() {
						return nil
					}
					relative, e := filepath.Rel(q.Dir, p)
					if e != nil {
						return e
					}
					return copyOne(relative, -1, true)
				})
				if err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			spool := q.Path(j.ID, ".spool.db")
			if _, err = os.Stat(spool); err == nil {
				relative := j.ID + ".spool.db"
				dst := filepath.Join(destination, relative)
				if err = snapshotSQLFile(ctx, spool, dst); err != nil {
					return err
				}
				f, e := hashFile(ctx, dst)
				if e != nil {
					return e
				}
				if err = add(relative, f); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if len(jobs) < 64 {
			break
		}
	}
	return atomicJSON(filepath.Join(destination, "backup-manifest.json"), manifest)
}

// ValidateAndRebindBackup runs only in an unpublished restore staging tree.
// A portable backup's original absolute source paths are never trusted.
func ValidateAndRebindBackup(ctx context.Context, directory, dataDirectory string) error {
	info, err := os.Lstat(filepath.Join(directory, "backup-manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		// Whole-directory backups made before portable task manifests are still
		// restorable. Preserve their bytes for offline recovery, but never expose
		// an unverified "done" file or resume a task with stale absolute paths.
		if err = os.Rename(directory, directory+"-legacy-unverified"); err != nil {
			return fmt.Errorf("preserve pre-manifest export files: %w", err)
		}
		q, e := Open(directory)
		if e != nil {
			return e
		}
		return q.Close()
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 32<<20 {
		return errors.New("invalid export backup manifest size/type")
	}
	b, err := os.ReadFile(filepath.Join(directory, "backup-manifest.json"))
	if err != nil {
		return fmt.Errorf("export queue backup manifest: %w", err)
	}
	if len(b) > 32<<20 {
		return errors.New("export backup manifest exceeds budget")
	}
	var manifest queueBackup
	if err = json.Unmarshal(b, &manifest); err != nil {
		return err
	}
	if manifest.Version != 1 || len(manifest.Files) > 65536 {
		return errors.New("unsupported export backup manifest")
	}
	seen := map[string]bool{}
	for _, expected := range manifest.Files {
		name := filepath.FromSlash(expected.Path)
		if !filepath.IsLocal(name) || strings.Contains(name, ":") || seen[name] {
			return errors.New("invalid export backup path")
		}
		seen[name] = true
		// Reject symlink components as well as a symlink leaf.
		for p := filepath.Join(directory, name); p != filepath.Clean(directory); p = filepath.Dir(p) {
			info, e := os.Lstat(p)
			if e != nil {
				return e
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("export backup symlink")
			}
		}
		actual, e := hashFile(ctx, filepath.Join(directory, name))
		if e != nil {
			return e
		}
		if actual.Bytes != expected.Bytes || actual.SHA256 != expected.SHA256 {
			return fmt.Errorf("export backup checksum mismatch: %s", expected.Path)
		}
	}
	if !seen["queue.db"] {
		return errors.New("export queue missing from backup manifest")
	}
	if err = filepath.WalkDir(directory, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(directory, p)
		if e != nil {
			return e
		}
		if rel != "backup-manifest.json" && !seen[rel] {
			return fmt.Errorf("unverified export backup file: %s", rel)
		}
		return nil
	}); err != nil {
		return err
	}
	q, err := Open(directory)
	if err != nil {
		return err
	}
	defer q.Close()
	absolute, err := filepath.Abs(dataDirectory)
	if err != nil {
		return err
	}
	cursor := ""
	for {
		rows, err := q.db.QueryContext(ctx, "SELECT id,body FROM jobs WHERE id>? ORDER BY id LIMIT 64", cursor)
		if err != nil {
			return err
		}
		var jobs []*Job
		for rows.Next() {
			var id string
			var body []byte
			if err = rows.Scan(&id, &body); err != nil {
				break
			}
			cursor = id
			var j *Job
			j, err = decodeJob(body)
			if err != nil {
				break
			}
			if !ValidID(id) || id != j.ID {
				err = errors.New("invalid restored task identity")
				break
			}
			jobs = append(jobs, j)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Status == "done" && !seen[j.ID+".csv"] {
				return errors.New("ready task is missing its verified file")
			}
			if j.Committed.Offset > 0 && j.Status != "done" && j.Status != "failed" && j.Status != "cancelled" && j.Status != "expired" {
				if !seen[j.ID+".csv.part"] && !seen[j.ID+".csv"] {
					return errors.New("restored task is missing its committed prefix")
				}
			}
			j.Source.DSN = filepath.Join(absolute, "metrics.db")
			j.Source.Telemetry.ArchiveDirectory = filepath.Join(absolute, "telemetry")
			if j.Status == "running" || j.Status == "publishing" || j.Status == "retry_wait" {
				j.Status = "recovering"
				j.WaitReason = "checkpoint_validation"
			}
			if err = q.Save(ctx, j); err != nil {
				return err
			}
		}
		if len(jobs) < 64 {
			break
		}
	}
	// This is evidence for the immutable package, not a valid manifest of the
	// rebound queue database. Do not carry it into a subsequent live snapshot.
	return os.Remove(filepath.Join(directory, "backup-manifest.json"))
}
