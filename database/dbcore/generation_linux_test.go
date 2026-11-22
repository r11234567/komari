//go:build linux

package dbcore

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestGenerationRenameAcrossMountsLeavesBothTreesIntact(t *testing.T) {
	source := t.TempDir()
	destination, err := os.MkdirTemp("/dev/shm", "komari-generation-")
	if err != nil {
		t.Skipf("independent tmpfs unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(destination) })
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Sys().(*syscall.Stat_t).Dev == other.Sys().(*syscall.Stat_t).Dev {
		t.Skip("temporary directories are on the same mount")
	}
	entry := filepath.Join(source, "state")
	if err := os.WriteFile(entry, []byte("source-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveGenerationEntries(source, destination, []string{"state"}); !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("rename = %v, want EXDEV", err)
	}
	content, err := os.ReadFile(entry)
	if err != nil || string(content) != "source-only" {
		t.Fatalf("source entry changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "state")); !os.IsNotExist(err) {
		t.Fatalf("unexpected destination entry: %v", err)
	}
}
