//go:build !linux

package historyexport

import "os"

func limitFileReadAhead(_ *os.File) error { return nil }

// The production worker requires Linux. Keep shared CSV code buildable for
// other platforms' compile checks and non-production component tests.
func releaseFileCache(_ *os.File, _, through int64) (int64, error) {
	return through, nil
}
