package metric

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestArchiveSizeCountsOnlyPublishedRegularFiles(t *testing.T) {
	dir := t.TempDir()
	_, telemetry := telemetryFixture(t, dir)
	root := filepath.Join(dir, "archive")
	day := filepath.Join(root, "2026", "11", "24")
	require.NoError(t, os.MkdirAll(day, 0o700))
	for name, contents := range map[string]string{
		"shared.jsonl.zst":     "compressed bytes",
		"next.jsonl.zst":       "123",
		".stage.jsonl.zst":     "temporary",
		"saved.jsonl.zst.part": "partial",
		"other.db":             "separate",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(day, name), []byte(contents), 0o600))
	}
	outside := filepath.Join(dir, "outside.jsonl.zst")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	_ = os.Symlink(outside, filepath.Join(day, "linked.jsonl.zst"))
	_ = os.Symlink(dir, filepath.Join(day, "linked-directory"))
	size, err := telemetry.ArchiveSize(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, len("compressed bytes123"), size)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = telemetry.ArchiveSize(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
