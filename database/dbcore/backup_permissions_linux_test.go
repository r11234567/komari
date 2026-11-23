package dbcore

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"github.com/komari-monitor/komari/internal/restorearchive"
)

func TestAutomaticBackupStaysPrivateInExistingDirectory(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	backupDir := filepath.Join(root, "backup")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "state"), []byte("live state"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := writePreRestoreSnapshotFrom(dataDir, dataDir, filepath.Join(dataDir, "backup.zip"), "")
	if err != nil {
		t.Fatal(err)
	}

	assertPrivateArchive := func() {
		t.Helper()
		info, err := os.Stat(archive)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(archive) != backupDir || info.Mode().Perm() != 0o600 {
			t.Fatalf("archive path/mode = %s/%04o", archive, info.Mode().Perm())
		}
		reader, err := zip.OpenReader(archive)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if len(reader.File) != 2 || reader.File[0].Name != restorearchive.MarkupName || reader.File[1].Name != "state" {
			t.Fatalf("unexpected archive contents: %v", reader.File)
		}
	}
	assertPrivateArchive()

	// An existing readable ZIP must be replaced with a new private file.
	if err := os.Chmod(archive, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backupDataDirectory(dataDir, archive, nil, false); err != nil {
		t.Fatal(err)
	}
	assertPrivateArchive()
}
