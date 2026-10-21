package privilegeddelivery

import (
	"github.com/komari-monitor/komari/database/clients"
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

func TestRestorationAfterLostFinalACKRetainsHistoryAndRejectsDelayedSuccess(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 1, RemoteControlEnabled: true, WebSSHEnabled: true})
	prepare(t, db, id, r)
	localSuccess(t, db, id)
	o := observation(r)
	o.OperationId = "operation"
	_, err := Observe(db, id, "daemon-before-restoration", o)
	require.NoError(t, err)
	before, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	allowed, err := clients.PrivilegedCapabilityAllowedWithDB(db, id, "webssh")
	require.NoError(t, err)
	require.True(t, allowed)
	request := &hostv1.ReportHostOperationRequest{AgentId: id, OperationId: "operation", ReceiptId: "restoration-after-lost-ack", Phase: hostv1.HostOperationPhase_HOST_OPERATION_PHASE_RESTORED, Error: "local administrator restored original service", Operator: "admin", LocalAuthentication: "pam"}
	_, err = ReportOperation(db, request)
	require.NoError(t, err)
	// Retrying a lost restoration ACK preserves the immutable receipt.
	_, err = ReportOperation(db, request)
	require.NoError(t, err)
	after, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Equal(t, StateFailed, after.Desired.State)
	require.Equal(t, hostv1.HostOperationPhase_HOST_OPERATION_PHASE_RESTORED, after.Operation.Phase)
	require.Equal(t, before.State.AppliedRevision, after.State.AppliedRevision, "restoration cannot invent an applied snapshot")
	allowed, err = clients.PrivilegedCapabilityAllowedWithDB(db, id, "webssh")
	require.NoError(t, err)
	require.False(t, allowed, "historical applied snapshot cannot dispatch new sessions after restoration")
	_, err = Observe(db, id, "delayed-daemon-success", o)
	require.Error(t, err, "old observation cannot mark restored state delivered again")
}
