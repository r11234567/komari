// Package backup validates and stages Komari backup archives for startup restore.
package backup

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/internal/datadir"
	"github.com/komari-monitor/komari/internal/restorearchive"
)

var restoreMutex sync.Mutex

const (
	MaxArchiveSize       = restorearchive.MaxSize
	maxMultipartBodySize = MaxArchiveSize + (1 << 20)
)

// RestoreLock prevents a second upload from replacing the already validated
// backup while the first request is waiting for the process to restart.
type RestoreLock struct {
	once sync.Once
}

func AcquireRestoreLock() (*RestoreLock, error) {
	if !restoreMutex.TryLock() {
		return nil, fmt.Errorf("another restore operation is already in progress")
	}
	return &RestoreLock{}, nil
}

func (l *RestoreLock) Release() {
	l.once.Do(restoreMutex.Unlock)
}

// StageHTTPUpload stages a multipart backup and returns the lock to the
// caller on success. Hold it until restart so another upload cannot replace it.
func StageHTTPUpload(w http.ResponseWriter, r *http.Request) (*RestoreLock, int, error) {
	tooLarge := func() error {
		return fmt.Errorf("backup upload exceeds the %d byte request limit", maxMultipartBodySize)
	}
	if r.ContentLength > maxMultipartBodySize {
		return nil, http.StatusRequestEntityTooLarge, tooLarge()
	}

	lock, err := AcquireRestoreLock()
	if err != nil {
		return nil, http.StatusConflict, err
	}
	staged := false
	defer func() {
		if !staged {
			lock.Release()
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, maxMultipartBodySize)
	file, header, err := r.FormFile("backup")
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return nil, http.StatusRequestEntityTooLarge, tooLarge()
		}
		return nil, http.StatusBadRequest, fmt.Errorf("get uploaded backup: %w", err)
	}
	defer file.Close()
	if err := lock.SaveUploadedBackup(file, header.Filename); err != nil {
		return nil, http.StatusBadRequest, err
	}
	staged = true
	return lock, http.StatusOK, nil
}

func (l *RestoreLock) SaveUploadedBackup(file io.Reader, filename string) error {
	if err := dbcore.SupportsAtomicRestore(); err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToLower(filename), ".zip") {
		return fmt.Errorf("uploaded file must be a ZIP archive")
	}
	if err := os.MkdirAll("./data", 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	tempFile, err := os.CreateTemp("./data", ".backup-upload-*.zip")
	if err != nil {
		return fmt.Errorf("create temporary backup: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	written, err := io.Copy(tempFile, io.LimitReader(file, MaxArchiveSize+1))
	if err != nil {
		tempFile.Close()
		return fmt.Errorf("save uploaded backup: %w", err)
	}
	if written > MaxArchiveSize {
		tempFile.Close()
		return fmt.Errorf("backup archive exceeds the %d byte limit", MaxArchiveSize)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync uploaded backup: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close uploaded backup: %w", err)
	}
	if err := ValidateArchive(tempPath); err != nil {
		return err
	}

	finalPath := filepath.Join(".", "data", "backup.zip")
	if err := datadir.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("publish uploaded backup: %w", err)
	}
	return datadir.SyncDirectory(filepath.Dir(finalPath))
}

// ValidateArchive checks the portable backup layout before staging it.
func ValidateArchive(archivePath string) error {
	reader, err := restorearchive.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open backup archive: %w", err)
	}
	defer reader.Close()
	_, err = reader.RestoreEntries(true)
	return err
}
