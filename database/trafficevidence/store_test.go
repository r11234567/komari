package trafficevidence

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTrafficEvidenceSurvivesRestartAndExpiresFromCaptureTime(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "evidence", "store.db")
	now := time.Now().UTC()
	store, err := Open(path)
	require.NoError(t, err)
	early := now.Add(-time.Minute)
	require.NoError(t, store.Put(ctx, "node-a", Incident, "a", early, early.Add(IncidentRetention), []byte("collecting"), []byte("initial detail"), false))
	require.NoError(t, store.Put(ctx, "node-a", Incident, "a", now, now.Add(IncidentRetention), []byte("summary"), []byte("192.0.2.2"), true))
	require.NoError(t, store.Put(ctx, "node-a", Incident, "a", now.Add(time.Minute), now.Add(30*24*time.Hour), []byte("changed"), []byte("changed"), true))
	require.NoError(t, store.Put(ctx, "node-a", Hour, "h", now, now.Add(HourRetention), []byte("old hour"), nil, false))
	require.NoError(t, store.Put(ctx, "node-a", Hour, "h", now.Add(time.Minute), now.Add(HourRetention), []byte("new hour"), nil, false))
	require.NoError(t, store.Put(ctx, "node-a", Hour, "h", now, now.Add(HourRetention), []byte("obsolete hour"), nil, false))
	require.NoError(t, store.Close())

	store, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	rows, err := store.List(ctx, "node-a", Incident, now)
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("summary")}, rows)
	rows, err = store.List(ctx, "node-a", Hour, now)
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("new hour")}, rows)
	raw, err := store.Detail(ctx, "node-a", "a", now)
	require.NoError(t, err)
	require.Equal(t, "192.0.2.2", string(raw))
	_, err = store.Detail(ctx, "node-b", "a", now)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, err = store.Detail(ctx, "node-a", "a", now.Add(IncidentRetention))
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, store.Prune(ctx, now.Add(IncidentRetention)))
	_, err = store.Detail(ctx, "node-a", "a", now)
	require.ErrorIs(t, err, sql.ErrNoRows)
}
