package restorearchive

import (
	"archive/zip"
	"bytes"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCopyToChecksDeclaredSizeAndCRC(t *testing.T) {
	payload := []byte("archive entry")
	for _, tc := range []struct {
		name string
		size uint64
		crc  uint32
		ok   bool
	}{
		{"valid", uint64(len(payload)), crc32.ChecksumIEEE(payload), true},
		{"wrong size", uint64(len(payload) - 1), crc32.ChecksumIEEE(payload), false},
		{"wrong checksum", uint64(len(payload)), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := zip.NewWriter(&archive)
			entry, err := writer.CreateRaw(&zip.FileHeader{
				Name: "komari.db", Method: zip.Store, CRC32: tc.crc,
				CompressedSize64: uint64(len(payload)), UncompressedSize64: tc.size,
			})
			require.NoError(t, err)
			_, err = entry.Write(payload)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			reader, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
			require.NoError(t, err)
			var out bytes.Buffer
			err = (Entry{File: reader.File[0], Path: "komari.db"}).CopyTo(&out)
			if tc.ok {
				require.NoError(t, err)
				require.Equal(t, payload, out.Bytes())
			} else {
				require.Error(t, err)
				require.LessOrEqual(t, uint64(out.Len()), tc.size)
			}
		})
	}
}

func TestReadBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.zip")
	out, err := os.Create(path)
	require.NoError(t, err)
	writer := zip.NewWriter(out)
	for _, name := range []string{"a.txt", "b.txt"} {
		entry, err := writer.Create(name)
		require.NoError(t, err)
		_, err = io.WriteString(entry, strings.Repeat("fixture\n", 512))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	require.NoError(t, out.Close())

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	info, err := file.Stat()
	require.NoError(t, err)
	const expandedSize = 8192
	require.Less(t, info.Size(), int64(expandedSize/2))

	_, err = readBounded(file, info.Size()-1, 2)
	require.ErrorContains(t, err, "regular archive")
	_, err = readBounded(file, expandedSize, 1)
	require.ErrorContains(t, err, "too many files")
	_, err = readBounded(file, expandedSize-1, 2)
	require.ErrorContains(t, err, "expands beyond")
	bounded, err := readBounded(file, expandedSize, 2)
	require.NoError(t, err)
	require.Len(t, bounded.File, 2)
}
