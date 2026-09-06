// Package fsmove relocates directories across filesystem boundaries.
//
// os.Rename is the natural way to swap a directory into place, and it is atomic
// when both paths share a filesystem. In a container they often do not: the
// data directory is typically a bind mount or volume, so renaming it - or
// renaming anything into it from a staged path elsewhere - fails with EXDEV,
// and a busy mount point can fail with EBUSY. Restore flows built on rename
// then break on exactly the deployments that most need them, and their rollback
// arms, built the same way, fail too.
package fsmove

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Directory moves source to destination, which must not already exist.
//
// A rename is attempted first, so the common same-filesystem case stays atomic
// and cheap. When the kernel reports the paths are not renameable into each
// other, the tree is copied entry by entry and the source removed, preserving
// symlinks and permission bits.
//
// The fallback is not atomic. A failure partway leaves destination partially
// written, so callers that need to roll back should move the original aside
// rather than delete it, and should treat destination as suspect on error.
func Directory(source, destination string) error {
	if source == destination {
		return nil
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("move %s: destination %s already exists", source, destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect destination %s: %w", destination, err)
	}

	if err := os.Rename(source, destination); err == nil {
		return nil
	} else if !isCrossDeviceError(err) {
		return err
	}

	if err := copyTree(source, destination); err != nil {
		// Leave the source untouched: it is still the only complete copy.
		return fmt.Errorf("copy %s to %s: %w", source, destination, err)
	}
	if err := os.RemoveAll(source); err != nil {
		return fmt.Errorf("remove %s after copying to %s: %w", source, destination, err)
	}
	return nil
}

// isCrossDeviceError reports whether a rename failed for a reason that copying
// can work around, rather than a genuine error worth surfacing.
func isCrossDeviceError(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY) ||
		errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EPERM)
}

func copyTree(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		// Recreate the link rather than following it: the target may be outside
		// the tree, or may not exist yet.
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(target, destination)

	case info.IsDir():
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyTree(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		// Directory permissions are applied after its contents, since a
		// read-only mode would otherwise block writing them.
		return os.Chmod(destination, info.Mode().Perm())

	case info.Mode().IsRegular():
		return copyFile(source, destination, info.Mode().Perm())

	default:
		// Sockets, devices and pipes carry no state worth moving; skipping one
		// is better than failing an entire restore over it.
		return nil
	}
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
