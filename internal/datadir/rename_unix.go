//go:build !windows

package datadir

import "os"

func Rename(source, destination string) error {
	return os.Rename(source, destination)
}
