//go:build !windows

package historyexport

import (
	"errors"
	"os"
	"syscall"
)

func workLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

func tryWorkLock(f *os.File, exclusive bool) error {
	how := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	return syscall.Flock(int(f.Fd()), how)
}
func unlockWork(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
