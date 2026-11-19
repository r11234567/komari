//go:build linux

package historyexport

import (
	"os"

	"golang.org/x/sys/unix"
)

// Explicit 64 KiB reads already bound the recovery buffer. Suppress kernel
// read-ahead as well, so unread pages cannot fill the worker's memory budget.
func limitFileReadAhead(f *os.File) error {
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_RANDOM)
}

// Release complete pages already consumed by this sequential export. Writes
// must be synced first. Keep the trailing partial page for the next batch.
func releaseFileCache(f *os.File, from, through int64) (int64, error) {
	page := int64(os.Getpagesize())
	start, end := from/page*page, through/page*page
	if end <= start {
		return from, nil
	}
	if err := unix.Fadvise(int(f.Fd()), start, end-start, unix.FADV_DONTNEED); err != nil {
		return from, err
	}
	return end, nil
}
