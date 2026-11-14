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
	"strings"
	"time"
)

// ValidateTelemetryBackup checks an isolated, already extracted bundle before
// startup publishes it. It never opens a writable Store or repairs the input.
// Legacy SQL-only backups remain supported by the explicit migration reader.
func ValidateTelemetryBackup(ctx context.Context, databasePath, archiveDirectory string) error {
	db, err := sql.Open("sqlite3", setSQLiteDSNParam(setSQLiteDSNParam(sqliteFileDSN(databasePath), "mode", "ro"), "_txlock", "deferred"))
	if err != nil {
		return err
	}
	defer db.Close()
	var check string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("restored telemetry SQLite integrity: %s", check)
	}
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE '%telemetry_progress'")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		tables = append(tables, name)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if re != nil {
		return re
	}
	if len(tables) == 0 {
		return nil
	}
	if len(tables) != 1 {
		return fmt.Errorf("ambiguous telemetry schemas in backup")
	}
	prefix := strings.TrimSuffix(tables[0], "progress")
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	var version int
	if err = db.QueryRowContext(ctx, "SELECT value FROM "+quote(prefix+"progress")+" WHERE task='schema_version'").Scan(&version); err != nil {
		return err
	}
	if version != 3 && version != 4 && version != 5 && version != 6 && version != 7 && version != 8 && version != 9 && version != 10 {
		return fmt.Errorf("unsupported restored telemetry schema %d", version)
	}
	root, err := os.OpenRoot(archiveDirectory)
	if err != nil {
		return fmt.Errorf("missing telemetry archive directory: %w", err)
	}
	defer root.Close()
	f, err := root.Open("manifest.json")
	if err != nil {
		return fmt.Errorf("missing telemetry backup manifest: %w", err)
	}
	defer f.Close()
	var manifest telemetryBackupManifest
	decoder := json.NewDecoder(io.LimitReader(f, 32<<20))
	if err = decoder.Decode(&manifest); err != nil {
		return err
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid telemetry manifest trailing data")
	}
	if manifest.Version != 1 || len(manifest.Files) > 100000 {
		return fmt.Errorf("unsupported or oversized telemetry manifest")
	}
	var identity string
	var through int64
	if err = db.QueryRowContext(ctx, "SELECT id FROM "+quote(prefix+"identity")+" WHERE slot=1").Scan(&identity); err != nil {
		return err
	}
	if err = db.QueryRowContext(ctx, "SELECT value FROM "+quote(prefix+"progress")+" WHERE task='ingest'").Scan(&through); err != nil {
		return err
	}
	if manifest.Store != identity || manifest.Through != through {
		return fmt.Errorf("telemetry manifest/SQL boundary mismatch")
	}
	files := make(map[string]telemetryBackupFile, len(manifest.Files))
	for _, file := range manifest.Files {
		if file.Path == "" || !filepath.IsLocal(filepath.FromSlash(file.Path)) || strings.Contains(file.Path, `\`) || filepath.ToSlash(filepath.Clean(file.Path)) != file.Path || file.Bytes < 0 || (file.SHA256 != "" && len(file.SHA256) != 64) {
			return fmt.Errorf("invalid telemetry manifest file")
		}
		if _, ok := files[file.Path]; ok {
			return fmt.Errorf("duplicate telemetry manifest file")
		}
		files[file.Path] = file
	}
	dayColumn := "0"
	if version >= 8 {
		dayColumn = "archive_day"
	}
	fileColumns := "'',0"
	if version >= 10 {
		fileColumns = "archive_file,archive_offset"
	}
	rows, err = db.QueryContext(ctx, "SELECT id,checksum,file_bytes,created,"+dayColumn+","+fileColumns+" FROM "+quote(prefix+"segments")+" WHERE state IN ('archived','deleting','complete')")
	if err != nil {
		return err
	}
	defer rows.Close()
	expected := map[string]int64{}
	for rows.Next() {
		var id, hash, archiveFile string
		var size, created, archiveDay, offset int64
		if err = rows.Scan(&id, &hash, &size, &created, &archiveDay, &archiveFile, &offset); err != nil {
			return err
		}
		if len(id) != 32 {
			return fmt.Errorf("invalid restored segment identity")
		}
		if archiveFile != "" {
			if len(archiveFile) != 32 || hash != "" || offset < 0 || size < 0 || offset > telemetryArchiveFileLimit-size {
				return fmt.Errorf("invalid restored archive extent")
			}
			id = archiveFile
		}
		day := time.Unix(0, created).UTC()
		if archiveDay != 0 {
			day = time.Unix(archiveDay, 0).UTC()
		}
		rel := day.Format("2006/01/02") + "/" + id + ".jsonl.zst"
		file, ok := files[rel]
		if !ok || (archiveFile == "" && file.Bytes != size) || file.Bytes < offset+size || file.SHA256 != hash {
			return fmt.Errorf("telemetry manifest/catalog mismatch: %s", id)
		}
		expected[rel] = max(expected[rel], offset+size)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for rel, size := range expected {
		file := files[rel]
		if file.Bytes != size {
			return fmt.Errorf("telemetry manifest extent coverage mismatch")
		}
		info, err := root.Lstat(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() != size {
			return fmt.Errorf("restored archive type/size mismatch: %s", rel)
		}
		if file.SHA256 == "" {
			delete(files, rel)
			continue
		}
		f, err := root.Open(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, &telemetryContextReader{ctx: ctx, r: f})
		f.Close()
		if err != nil {
			return err
		}
		if n != size || hex.EncodeToString(h.Sum(nil)) != file.SHA256 {
			return fmt.Errorf("restored archive checksum mismatch: %s", rel)
		}
		delete(files, rel)
	}
	if len(files) != 0 {
		return fmt.Errorf("telemetry manifest contains files absent from SQL catalog")
	}
	return nil
}
