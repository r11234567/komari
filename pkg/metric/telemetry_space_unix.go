//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package metric

import "golang.org/x/sys/unix"

func telemetryFreeBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
