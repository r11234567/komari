package fsmove

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDirectoryMovesTreeWithinOneFilesystem(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src")
	nested := filepath.Join(source, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file.txt"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(root, "dst")
	if err := Directory(source, destination); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source must be gone after a move")
	}
	content, err := os.ReadFile(filepath.Join(destination, "a", "b", "file.txt"))
	if err != nil || string(content) != "payload" {
		t.Fatalf("content = %q err = %v, want the original payload", content, err)
	}
}

// The copy fallback is what runs when data/ is a bind mount, so it has to
// reproduce the tree faithfully rather than merely getting the bytes across.
func TestCopyTreePreservesSymlinksAndModes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(source, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub", "data.bin"), []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/data.bin", filepath.Join(source, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	destination := filepath.Join(root, "dst")
	if err := copyTree(source, destination); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	info, err := os.Stat(filepath.Join(destination, "sub", "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600: a restored database must not become world-readable", perm)
	}
	dirInfo, err := os.Stat(filepath.Join(destination, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o750 {
		t.Errorf("directory mode = %o, want 750", perm)
	}
	// A symlink must be recreated, not dereferenced into a second copy.
	linkInfo, err := os.Lstat(filepath.Join(destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was replaced by a regular file")
	}
	target, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil || target != "sub/data.bin" {
		t.Errorf("link target = %q err = %v, want sub/data.bin", target, err)
	}
}

func TestDirectoryRefusesToOverwrite(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src")
	destination := filepath.Join(root, "dst")
	for _, dir := range []string{source, destination} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := Directory(source, destination); err == nil {
		t.Fatal("moving onto an existing path must fail rather than merge or clobber")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("a refused move must leave the source intact")
	}
}

// Only errors a copy can work around may trigger the fallback; a genuine
// failure has to surface instead of being retried as a copy.
func TestCrossDeviceErrorClassification(t *testing.T) {
	for _, err := range []error{syscall.EXDEV, syscall.EBUSY, syscall.ENOTEMPTY, syscall.EPERM} {
		if !isCrossDeviceError(&os.LinkError{Err: err}) {
			t.Errorf("%v should trigger the copy fallback", err)
		}
	}
	for _, err := range []error{syscall.ENOENT, syscall.EACCES, syscall.ENOSPC} {
		if isCrossDeviceError(&os.LinkError{Err: err}) {
			t.Errorf("%v must surface rather than fall back to copying", err)
		}
	}
}
