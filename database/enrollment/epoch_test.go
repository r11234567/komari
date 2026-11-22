package enrollment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
)

func TestEpochMismatchRejectedUntilSignedRefreshUpgrades(t *testing.T) {
	t.Cleanup(func() { dbcore.SetCurrentSecurityEpoch(1) })
	db, token, record, now := rotationFixture(t)

	// Issue a valid epoch-1 pair first.
	issued1, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.NoError(t, err)

	clientID, err := resolveAccessTokenDB(db, issued1.AccessToken)
	require.NoError(t, err)
	require.Equal(t, record.Client, clientID)

	// Simulate panel security epoch advance after backup restore.
	dbcore.SetCurrentSecurityEpoch(2)
	_, err = resolveAccessTokenDB(db, issued1.AccessToken)
	require.ErrorIs(t, err, ErrEpochMismatch)

	// Valid non-revoked agent refreshes with its RT/key proof and advances to epoch 2.
	gen1 := issued1.Generation
	issued2, err := rotateVerifiedDB(context.Background(), db, record, issued1.RefreshToken, uuid.NewString(), &gen1, 2, func() time.Time { return now.Add(time.Minute) })
	require.NoError(t, err)
	clientID, err = resolveAccessTokenDB(db, issued2.AccessToken)
	require.NoError(t, err)
	require.Equal(t, record.Client, clientID)

	var current models.AgentCredential
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.Equal(t, uint64(2), current.SecurityEpoch)
}
