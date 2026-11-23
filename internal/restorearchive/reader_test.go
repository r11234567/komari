package restorearchive

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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
