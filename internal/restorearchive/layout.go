package restorearchive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
)

type Entry struct {
	*zip.File
	Path string
}

// RestoreEntries validates paths before any extraction. Old complete backups
// may include files outside the application layout; those are not restored.
func (r *Reader) RestoreEntries(requireMarkup bool) ([]Entry, error) {
	var selected []Entry
	seen := make(map[string]struct{}, len(r.File))
	hasMarkup, hasDatabase := false, false
	for _, file := range r.File {
		raw := strings.TrimSuffix(file.Name, "/")
		name := path.Clean(raw)
		if raw == "" || name != raw || name == "." || strings.ContainsAny(name, "\\:\x00") ||
			!filepath.IsLocal(filepath.FromSlash(name)) || name == ".." || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("invalid backup path %q", file.Name)
		}
		for _, part := range strings.Split(name, "/") {
			if strings.TrimRight(part, " .") != part {
				return nil, fmt.Errorf("invalid backup path %q", file.Name)
			}
		}
		key := strings.ToLower(name)
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("duplicate backup path %q", file.Name)
		}
		seen[key] = struct{}{}
		if !file.Mode().IsRegular() && !file.Mode().IsDir() {
			return nil, fmt.Errorf("unsupported backup file type %q", file.Name)
		}
		if name == MarkupName && file.Mode().IsRegular() {
			hasMarkup = true
		}
		if name == "komari.db" && file.Mode().IsRegular() {
			hasDatabase = true
		}
		if restorable(name, file.Mode().IsDir()) {
			selected = append(selected, Entry{File: file, Path: name})
		}
	}
	if requireMarkup && !hasMarkup {
		return nil, errors.New("invalid backup file: missing komari-backup-markup file")
	}
	if !hasDatabase {
		return nil, errors.New("invalid backup file: missing komari.db")
	}
	return selected, nil
}

func restorable(name string, dir bool) bool {
	top, tail, nested := strings.Cut(name, "/")
	switch top {
	case MarkupName, "komari.db", "komari.db-wal", "komari.db-shm",
		"metrics.db", "metrics.db-wal", "metrics.db-shm", "secret.key",
		"GeoLite2-Country.mmdb", "favicon.ico", "return-route-signatures.json", "return-route-bgp-prefixes.json":
		return !nested && !dir
	case "theme", "telemetry", "plugin-data", "tls", "exports-v2", "external-database", "notification":
		return nested || dir
	case "plugin":
		child, _, _ := strings.Cut(tail, "/")
		// Recovery journals and package receipts can enable plugins on startup.
		if strings.HasPrefix(child, ".") || strings.EqualFold(path.Base(name), ".komari-package.json") {
			return false
		}
		return nested || dir
	default:
		return false
	}
}

// CopyTo limits decompression to the checked entry budget plus one byte,
// which also allows the ZIP reader to verify its checksum at EOF.
func (e Entry) CopyTo(out io.Writer) error {
	input, err := e.Open()
	if err != nil {
		return err
	}
	expected := int64(e.UncompressedSize64)
	n, copyErr := io.Copy(out, io.LimitReader(input, expected+1))
	if err := errors.Join(copyErr, input.Close()); err != nil {
		return err
	}
	if n != expected {
		return fmt.Errorf("backup entry %q differs from declared size", e.Path)
	}
	return nil
}
