package metric

import (
	"context"
	"errors"
	"io/fs"
	"strings"
)

// ArchiveSize measures published archive files once each, regardless of how
// many catalog entries share a file.
func (t *Telemetry) ArchiveSize(ctx context.Context) (int64, error) {
	if t == nil {
		return 0, nil
	}
	var size int64
	err := fs.WalkDir(t.root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Archive expiry can remove a file or an empty directory mid-walk.
		if errors.Is(err, fs.ErrNotExist) && path != "." {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".jsonl.zst") {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err == nil && info.Mode().IsRegular() {
			size += info.Size()
		}
		return err
	})
	return size, err
}
