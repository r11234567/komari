package privilegeddelivery

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestWorkspaceStablePrivilegedReadIsActuallyReadOnly(t *testing.T) {
	db, id := fixtureV2(t)
	saved, approved := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 2})
	// SQLite itself rejects all writes. This test fails against the former
	// GetState implementation's repeated initialization INSERT, even on conflict.
	require.NoError(t, db.Exec("PRAGMA query_only = ON").Error)
	for i := 0; i < 20; i++ {
		state, err := GetStateWithDB(db, id)
		require.NoError(t, err)
		require.Equal(t, saved.Revision, state.State.DesiredRevision)
		require.NotNil(t, state.Desired)
		require.Equal(t, approved.UpdateSequence, state.Desired.UpdateSequence)
	}
	// Prove the fixture's read-only restriction is real, not just an empty log.
	require.ErrorContains(t, db.Exec("UPDATE client_privileged_states SET update_sequence = update_sequence + 1 WHERE client = ?", id).Error, "readonly")
}
