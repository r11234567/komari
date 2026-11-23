package datadir

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

const (
	Root        = "./data"
	Generations = "generations"
	Manifest    = "current.json"
	Version     = 3
)

var ErrPendingLegacyRestore = errors.New("previous restore must finish before opening data")

// Pointer is the only publication record for a generation. Legacy version 2
// journals are read for transition from installations that used flat paths.
type Pointer struct {
	Version            int    `json:"version"`
	Generation         string `json:"active_generation"`
	PreviousGeneration string `json:"previous_generation,omitempty"`
	Epoch              uint64 `json:"security_epoch"`
	State              string `json:"state,omitempty"`
}

type Layout struct {
	Root    string
	Dir     string
	Pointer Pointer
}

func Open(root string) (Layout, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, err
	}
	layout := Layout{Root: root, Dir: root}
	raw, err := os.ReadFile(filepath.Join(root, Manifest))
	if os.IsNotExist(err) {
		return layout, nil
	}
	if err != nil {
		return Layout{}, err
	}
	if err := json.Unmarshal(raw, &layout.Pointer); err != nil {
		return Layout{}, fmt.Errorf("decode data generation: %w", err)
	}
	p := layout.Pointer
	switch p.Version {
	case 1, 2:
		if p.State != "active" {
			return Layout{}, ErrPendingLegacyRestore
		}
	case Version:
		if p.State != "" || p.Epoch == 0 || !ValidGeneration(p.Generation) {
			return Layout{}, errors.New("invalid active data generation")
		}
		if p.PreviousGeneration != "" && (!ValidGeneration(p.PreviousGeneration) || p.PreviousGeneration == p.Generation) {
			return Layout{}, errors.New("invalid previous data generation")
		}
		layout.Dir = filepath.Join(root, Generations, p.Generation)
		for _, dir := range []string{filepath.Dir(layout.Dir), layout.Dir} {
			info, err := os.Lstat(dir)
			if err != nil {
				return Layout{}, fmt.Errorf("open active generation: %w", err)
			}
			if !info.IsDir() {
				return Layout{}, fmt.Errorf("active generation path is not a directory: %s", dir)
			}
		}
		info, err := os.Lstat(filepath.Join(layout.Dir, "komari.db"))
		if err != nil {
			return Layout{}, fmt.Errorf("open active generation database: %w", err)
		}
		if !info.Mode().IsRegular() {
			return Layout{}, errors.New("active generation database is not a regular file")
		}
	default:
		return Layout{}, fmt.Errorf("unsupported data generation version %d", p.Version)
	}
	return layout, nil
}

func ValidGeneration(name string) bool {
	id, ok := strings.CutPrefix(name, "gen-")
	return ok && id != "" && strings.Trim(id, "0123456789abcdef-") == ""
}

func (l Layout) Resolve(path string) string {
	if path == "" || l.Dir == l.Root {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(l.Root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	switch first {
	case Generations, Manifest, "security-epoch.json", "backup.zip", ".restore-inbox", ".restore-rejected", ".restore-lock", ".uploading", ".pre-restore-backups":
		return path
	}
	return filepath.Join(l.Dir, rel)
}

var pinned atomic.Pointer[Layout]

// Pin selects the layout before any process-owned stores are opened.
func Pin(layout Layout) { pinned.Store(&layout) }

func Resolve(path string) string {
	if layout := pinned.Load(); layout != nil {
		return layout.Resolve(path)
	}
	return path
}

func SyncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// ResolveSQLite keeps URI options and escapes the replacement path.
func ResolveSQLite(dsn string) string {
	raw, isURI := strings.CutPrefix(dsn, "file:")
	path, query, hasQuery := strings.Cut(raw, "?")
	decoded := path
	if isURI {
		parsed, err := url.PathUnescape(path)
		if err != nil {
			return dsn
		}
		decoded = parsed
	}
	resolved := Resolve(decoded)
	if resolved == decoded {
		return dsn
	}
	if isURI {
		resolved = (&url.URL{Path: filepath.ToSlash(resolved)}).EscapedPath()
		resolved = "file:" + resolved
	}
	if hasQuery {
		resolved += "?" + query
	}
	return resolved
}
