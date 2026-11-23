// Package restorearchive bounds backup archives at upload and startup.
package restorearchive

import (
	"archive/zip"
	"fmt"
	"os"
)

const (
	MarkupName       = "komari-backup-markup"
	MaxSize    int64 = 4 << 30
	maxEntries       = 100_000
)

type Reader struct {
	*zip.Reader
	file *os.File
}

func (r *Reader) Close() error { return r.file.Close() }

// Open checks the archive size and expanded size before any payload is read.
func Open(path string) (*Reader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	reader, err := readBounded(file, MaxSize, maxEntries)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Reader{Reader: reader, file: file}, nil
}

func readBounded(file *os.File, maxSize int64, maxCount int) (*zip.Reader, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSize {
		return nil, fmt.Errorf("backup must be a regular archive of at most %d bytes", maxSize)
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		return nil, err
	}
	if len(reader.File) > maxCount {
		return nil, fmt.Errorf("backup archive has too many files: %d", len(reader.File))
	}
	budget := uint64(maxSize)
	for _, entry := range reader.File {
		if entry.UncompressedSize64 > budget {
			return nil, fmt.Errorf("backup archive expands beyond the %d byte limit", maxSize)
		}
		budget -= entry.UncompressedSize64
	}
	return reader, nil
}
