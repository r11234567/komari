package privilegeddelivery

import (
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSupersededHostRestorationIsArchivedWithoutChangingNewTarget(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 1})
	prepare(t, db, id, r)
	_, err := SaveWithDB(db, id, Settings{RequiredPrivilegeMode: 2}, 1, "supersede")
	require.NoError(t, err)
	before, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	_, err = ReportOperation(db, &hostv1.ReportHostOperationRequest{AgentId: id, OperationId: "operation", ReceiptId: "late-success", Phase: 3, LocalAuthentication: "pam", Operator: "operator"})
	require.Error(t, err)
	op, err := ReportOperation(db, &hostv1.ReportHostOperationRequest{AgentId: id, OperationId: "operation", ReceiptId: "late-restore", Phase: 4, Error: "old profile restored"})
	require.NoError(t, err)
	require.Equal(t, hostv1.HostOperationPhase(4), op.Phase)
	after, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Equal(t, before.Desired, after.Desired)
	require.Equal(t, before.State.AppliedRevision, after.State.AppliedRevision)
	require.Equal(t, before.State.UpdateSequence, after.State.UpdateSequence)
}
func TestNodeStateShowsHostProgressSeparatelyFromApplied(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 1})
	prepare(t, db, id, r)
	snap, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	require.NotNil(t, snap.Operation)
	require.Equal(t, hostv1.HostOperationPhase(1), snap.Operation.Phase)
	require.Zero(t, snap.State.AppliedRevision)
	localSuccess(t, db, id)
	snap, err = GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Equal(t, hostv1.HostOperationPhase(3), snap.Operation.Phase)
	require.Zero(t, snap.State.AppliedRevision)
}
