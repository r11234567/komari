//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package metric

// These targets retain transaction/SQL-full backpressure; available-space
// probing is supported on the deployed Unix and Windows platforms above.
func telemetryFreeBytes(string) (uint64, error) { return ^uint64(0), nil }
