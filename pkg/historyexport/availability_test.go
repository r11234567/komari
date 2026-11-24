package historyexport

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQueueDistinguishesMissingWorkerFromUnavailableWorker(t *testing.T) {
	queue := Queue{Dir: t.TempDir()}
	file := filepath.Join(queue.Dir, "worker.json")
	status := queue.Availability()
	require.False(t, status.Detected)
	require.False(t, status.Available)

	require.NoError(t, os.WriteFile(file, []byte("invalid heartbeat"), 0600))
	status = queue.Availability()
	require.True(t, status.Detected)
	require.False(t, status.Available)

	require.NoError(t, liveJSON(file, Availability{At: time.Now().Add(-time.Minute), Available: true}))
	status = queue.Availability()
	require.True(t, status.Detected)
	require.False(t, status.Available)

	require.NoError(t, liveJSON(file, Availability{At: time.Now(), Error: "worker quota unavailable"}))
	status = queue.Availability()
	require.True(t, status.Detected)
	require.False(t, status.Available)
	require.Equal(t, "worker quota unavailable", status.Error)

	require.NoError(t, liveJSON(file, Availability{At: time.Now(), Available: true}))
	status = queue.Availability()
	require.True(t, status.Detected)
	require.True(t, status.Available)
}
