package connectapi

import (
	"testing"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/web/rescueapp"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	rescuev1 "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRescueAndRemoteControlCoexistAfterDaemonACK(t *testing.T) {
	id, _, err := clients.CreateClient()
	require.NoError(t, err)
	db := dbcore.GetDBInstance()
	_, err = privilegeddelivery.Negotiate(db, id, "coexist.service", "before", 2, []int32{1, 2, 3})
	require.NoError(t, err)
	settings := privilegeddelivery.Settings{RequiredPrivilegeMode: 2, RemoteControlEnabled: true, WebSSHEnabled: true, RescueHelperEnabled: true}
	saved, err := privilegeddelivery.Save(id, settings, 0, "independent rescue service")
	require.NoError(t, err)
	approved, err := privilegeddelivery.Confirm(id, saved.Revision, "admin")
	require.NoError(t, err)
	authorization, err := privilegeddelivery.Authorization(approved)
	require.NoError(t, err)
	require.Equal(t, []hostv1.HostAction{hostv1.HostAction_HOST_ACTION_INSTALL_RESCUE_HELPER}, authorization.Actions)
	require.Equal(t, reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_NON_ROOT, authorization.TargetPrivilegeMode)
	_, err = privilegeddelivery.PrepareOperation(db, &hostv1.PrepareHostOperationRequest{AgentId: id, OperationId: "install-rescue", Revision: approved.Revision, ConfigHash: approved.ConfigHash, ApprovalGeneration: approved.ApprovalGeneration, Nonce: approved.Nonce})
	require.NoError(t, err)
	_, err = privilegeddelivery.ReportOperation(db, &hostv1.ReportHostOperationRequest{AgentId: id, OperationId: "install-rescue", ReceiptId: "local-result", Phase: hostv1.HostOperationPhase_HOST_OPERATION_PHASE_LOCAL_APPLIED, Operator: "admin", LocalAuthentication: "pam"})
	require.NoError(t, err)
	// Installation and a helper heartbeat cannot grant authority ahead of ACK.
	require.NoError(t, rescueapp.ReportStatus(id, &rescuev1.RescueHelperStatus{Installed: true, GuardianRunning: true, HelperRunning: true, HelperInstanceId: "coexist-helper", ObservedAt: timestamppb.Now()}))
	require.Error(t, rescueapp.ValidateHelper(id, "coexist-helper"))
	_, err = rescueapp.Create(id, rescuev1.RescueAction_RESCUE_ACTION_RESTORE_NETWORK, nil, nil, 0, "before-ack", 0)
	require.Error(t, err)
	capability := func(enabled bool) *reportv1.CapabilityState { return &reportv1.CapabilityState{Available: enabled} }
	_, err = privilegeddelivery.Observe(db, id, "daemon-ack", &hostv1.DaemonObservation{Revision: approved.Revision, ConfigHash: approved.ConfigHash, ApprovalGeneration: approved.ApprovalGeneration, ServiceId: "coexist.service", StartupId: "after", PrivilegeMode: 2, ObservedAt: timestamppb.Now(), OperationId: "install-rescue", HelperInstalled: true, HelperRunning: true, HelperAuthorized: true, HelperLeaseReady: true, EffectiveCapabilities: &reportv1.AgentCapabilities{PrivilegeMode: 2, RemoteControl: capability(true), Webssh: capability(true), RescueHelper: capability(true)}})
	require.NoError(t, err)
	profile, savedProfile, err := clients.GetDeploymentProfile(id)
	require.NoError(t, err)
	require.True(t, savedProfile)
	require.True(t, profile.RemoteControlEnabled)
	require.True(t, profile.RescueEnabled)
	require.Equal(t, clients.AgentRuntimeIdentityServiceAccount, profile.RuntimeIdentity)
	for _, feature := range []string{"remote", "webssh", "rescue"} {
		allowed, err := clients.PrivilegedCapabilityAllowed(id, feature)
		require.NoError(t, err)
		require.True(t, allowed, feature)
	}
	require.NoError(t, rescueapp.ValidateHelper(id, "coexist-helper"))
	require.Error(t, rescueapp.ValidateHelper(id, "wrong-helper"))
	session, err := rescueapp.Create(id, rescuev1.RescueAction_RESCUE_ACTION_RESTORE_NETWORK, nil, nil, 0, "after-ack", 0)
	require.NoError(t, err)
	assignment, err := rescueapp.NextAssignment(id, "coexist-helper", "")
	require.NoError(t, err)
	require.Equal(t, session.SessionId, assignment.Session.SessionId)
	require.Equal(t, commonv1.OperationState_OPERATION_STATE_RUNNING, assignment.Session.State)
	require.NotNil(t, assignment.Envelope)
	// Withdrawing rescue blocks its leases and new work immediately, while
	// leaving the independently approved WebSSH capability available.
	settings.RescueHelperEnabled = false
	_, err = privilegeddelivery.Save(id, settings, approved.Revision, "withdraw rescue only")
	require.NoError(t, err)
	require.Error(t, rescueapp.ValidateHelper(id, "coexist-helper"))
	_, err = rescueapp.Create(id, rescuev1.RescueAction_RESCUE_ACTION_RESTORE_NETWORK, nil, nil, 0, "after-withdrawal", 0)
	require.Error(t, err)
	allowed, err := clients.PrivilegedCapabilityAllowed(id, "webssh")
	require.NoError(t, err)
	require.True(t, allowed)
}
