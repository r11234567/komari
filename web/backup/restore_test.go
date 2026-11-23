package backup

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTestArchive(t *testing.T, entries map[string]string) string {
	t.Helper()
	archivePath := filepath.Join(t.TempDir(), "backup.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return archivePath
}

func TestValidateArchiveAcceptsFullAndConfigurationPackages(t *testing.T) {
	for name, entries := range map[string]map[string]string{
		"config": {
			"komari.db":            "main",
			"komari-backup-markup": "config",
		},
		"full": {
			"komari.db":            "main",
			"metrics.db":           "history",
			"komari-backup-markup": "full",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateArchive(writeTestArchive(t, entries)); err != nil {
				t.Fatalf("ValidateArchive rejected package: %v", err)
			}
		})
	}
}

func TestValidateArchiveRejectsPackagesThatCouldWipeCurrentData(t *testing.T) {
	for name, entries := range map[string]map[string]string{
		"missing database": {"komari-backup-markup": "marker"},
		"missing marker":   {"komari.db": "main"},
		"path traversal": {
			"komari.db":            "main",
			"komari-backup-markup": "marker",
			"../outside":           "bad",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateArchive(writeTestArchive(t, entries)); err == nil {
				t.Fatal("ValidateArchive accepted unsafe package")
			}
		})
	}
}

func TestStageHTTPUploadRejectsOversizedRequestBeforeParsing(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.ContentLength = maxMultipartBodySize + 1
	lock, status, err := StageHTTPUpload(httptest.NewRecorder(), request)
	if lock != nil || status != http.StatusRequestEntityTooLarge || err == nil {
		t.Fatalf("oversized upload: lock=%v status=%d err=%v", lock, status, err)
	}

	request = httptest.NewRequest(http.MethodPost, "/", nil)
	lock, status, err = StageHTTPUpload(httptest.NewRecorder(), request)
	if lock != nil || status != http.StatusBadRequest || err == nil {
		t.Fatalf("invalid upload: lock=%v status=%d err=%v", lock, status, err)
	}
	next, err := AcquireRestoreLock()
	if err != nil {
		t.Fatalf("failed upload retained restore lock: %v", err)
	}
	next.Release()
}
