package connectapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/pkg/bindticket"
	"github.com/komari-monitor/komari/pkg/rpc"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	"github.com/komari-monitor/komari/web/rescueapp"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	"github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	deploymentv1 "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1"
	enrollmentv1 "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	"github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	"github.com/r11234567/komari-proto/gen/go/komari/host/v1/hostv1connect"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	rescuev1 "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDualEnd_ReenrollmentResetsPrivilegesAndRedispatchesRuntime(t *testing.T) {
	for _, previous := range []string{"managed", "legacy without credentials"} {
		t.Run(previous, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const operator = "reenrollment-admin"
			admin := rpc.NewContextWithMeta(ctx, &rpc.ContextMeta{Principal: rpc.NewUserPrincipal(operator)})
			id, _, err := clients.CreateClientWithName("preserve node name")
			require.NoError(t, err)
			db := dbcore.GetDBInstance()

			profile, _, err := clients.GetDeploymentProfile(id)
			require.NoError(t, err)
			profile.MemoryIncludeCache = true
			profile.EnableInterval, profile.Interval = true, 15
			profile.RuntimeIdentity = clients.AgentRuntimeIdentityPrivileged
			profile.RemoteControlEnabled, profile.RescueEnabled, profile.EnableGPU = true, true, true
			_, runtime, _, err := clients.SaveDeploymentProfileForDispatchForced(id, profile, true)
			require.NoError(t, err)
			acked, err := clients.CompleteDeploymentConfigDelivery(id, runtime.Revision, clients.DeploymentDeliveryApplied, "")
			require.NoError(t, err)
			require.True(t, acked)

			service := enrollmentv1connect.NewEnrollmentServiceClient(testServer.Client(), testServer.URL)
			register := func() (string, *enrollmentv1.AgentCredentials, ed25519.PrivateKey) {
				t.Helper()
				pub, key, err := ed25519.GenerateKey(rand.Reader)
				require.NoError(t, err)
				begin, err := service.BeginEnrollment(ctx, connect.NewRequest(&enrollmentv1.BeginEnrollmentRequest{
					AgentPublicKey: &securityv1.PublicKey{Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: pub, KeyId: uuid.NewString()},
				}))
				require.NoError(t, err)
				ticket, _, err := bindticket.IssueWithDurationAndEpoch(operator, accounts.SessionDigestKey(), bindticket.TicketDuration, dbcore.CurrentSecurityEpoch())
				require.NoError(t, err)
				approval := connect.NewRequest(&enrollmentv1.ApproveEnrollmentRequest{UserCode: begin.Msg.UserCode, Target: &enrollmentv1.ApproveEnrollmentRequest_ExistingAgentId{ExistingAgentId: id}})
				approval.Header().Set("X-Bind-Ticket", ticket)
				_, err = (&enrollmentAdminService{}).ApproveEnrollment(admin, approval)
				require.NoError(t, err)
				poll, err := service.PollEnrollment(ctx, connect.NewRequest(&enrollmentv1.PollEnrollmentRequest{DeviceCode: begin.Msg.DeviceCode}))
				require.NoError(t, err)
				require.Equal(t, id, poll.Msg.GetCredentials().GetAgentId())
				return begin.Msg.DeviceCode, poll.Msg.Credentials, key
			}

			var oldCredentials *enrollmentv1.AgentCredentials
			var oldRevision uint64
			if previous == "managed" {
				// Seed a previously enrolled installation, including applied and pending history.
				_, oldCredentials, _ = register()
				state, err := privilegeddelivery.GetState(id)
				require.NoError(t, err)
				oldRevision = state.Desired.Revision + 2
				require.NoError(t, db.Create(&models.ClientPrivilegedRevision{
					Client: id, Revision: oldRevision - 1, Config: "{\"remote_control_enabled\":true,\"webssh_enabled\":true,\"rescue_helper_enabled\":true}",
					State: privilegeddelivery.StateDelivered, ProtocolVersion: 2,
				}).Error)
				require.NoError(t, db.Create(&models.ClientPrivilegedRevision{
					Client: id, Revision: oldRevision, Config: "{\"remote_control_enabled\":true,\"webssh_enabled\":true,\"rescue_helper_enabled\":true}",
					State: privilegeddelivery.StateNeedsManualAuthorization, ProtocolVersion: 2, Nonce: "old-approval",
				}).Error)
				require.NoError(t, db.Model(&models.ClientPrivilegedState{}).Where("client = ?", id).Updates(map[string]any{
					"desired_revision": oldRevision, "applied_revision": oldRevision - 1, "service_id": "old-agent.service",
					"privilege_mode": 1, "startup_id": "old-boot", "observation": "{}",
				}).Error)
				require.NoError(t, clients.UpdateAppliedPrivilegedProfileWithDB(db, id, true, true, true, 1))
				_, _, runtime, err = clients.GetDeploymentProfileWithDelivery(id)
				require.NoError(t, err)
			}

			require.NoError(t, rescueapp.ReportStatus(id, &rescuev1.RescueHelperStatus{
				Installed: true, GuardianRunning: true, HelperRunning: true, HelperInstanceId: "old-helper",
			}))
			queued := models.RescueSession{ID: uuid.NewString(), Client: id, State: int32(commonv1.OperationState_OPERATION_STATE_QUEUED)}
			require.NoError(t, db.Create(&queued).Error)
			agentRuntime.SetConnectInstallation(id, 1, &reportv1.InstallationObservation{StartupId: "old-boot", ObservedAt: timestamppb.Now()})

			code, creds, key := register()
			retry, err := service.PollEnrollment(ctx, connect.NewRequest(&enrollmentv1.PollEnrollmentRequest{DeviceCode: code}))
			require.NoError(t, err)
			require.True(t, proto.Equal(creds, retry.Msg.Credentials))
			if oldCredentials != nil {
				_, err := enrollment.ResolveAccessToken(oldCredentials.AccessToken)
				require.Error(t, err)
			}
			state, err := privilegeddelivery.GetState(id)
			require.NoError(t, err)
			require.Nil(t, state.Applied)
			require.Empty(t, state.State.ServiceID)
			require.Empty(t, state.State.Observation)
			require.Equal(t, oldRevision+1, state.Desired.Revision)
			require.Equal(t, privilegeddelivery.Settings{}, state.Desired.Settings)
			if oldRevision != 0 {
				history, err := privilegeddelivery.History(id, 8)
				require.NoError(t, err)
				require.Equal(t, privilegeddelivery.StateSuperseded, history[1].State)
				require.Empty(t, history[1].Nonce)
				require.Equal(t, privilegeddelivery.StateDelivered, history[2].State)
			}

			panel, err := (&deploymentService{}).GetDeployment(admin, connect.NewRequest(&deploymentv1.GetDeploymentRequest{AgentId: id}))
			require.NoError(t, err)
			require.Nil(t, panel.Msg.Installation)
			require.False(t, panel.Msg.Profile.Install.RemoteControlEnabled)
			require.False(t, panel.Msg.RescueHelper.Installed)
			require.False(t, panel.Msg.RescueHelper.Requested)
			session, err := rescueapp.GetSession(queued.ID)
			require.NoError(t, err)
			require.Equal(t, commonv1.OperationState_OPERATION_STATE_CANCELLED, session.State)
			require.Error(t, rescueapp.ValidateTarget(id))

			transport := newDPoPTestClient(creds.AccessToken, key)
			ordinary := configv1connect.NewConfigServiceClient(transport, testServer.URL)
			desired, err := ordinary.GetDesiredConfig(ctx, connect.NewRequest(&configv1.GetDesiredConfigRequest{AgentId: id, AppliedRevision: runtime.Revision}))
			require.NoError(t, err)
			require.Equal(t, runtime.Revision+1, desired.Msg.GetDesired().GetRevision())
			require.True(t, desired.Msg.GetDesired().GetRuntime().GetMemoryIncludeCache())
			require.Equal(t, 15*time.Second, desired.Msg.GetDesired().GetRuntime().GetReportInterval().AsDuration())
			_, err = ordinary.AcknowledgeConfig(ctx, connect.NewRequest(&configv1.AcknowledgeConfigRequest{
				AgentId: id, Revision: desired.Msg.Desired.Revision, Status: configv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED,
			}))
			require.NoError(t, err)

			host := hostv1connect.NewHostServiceClient(transport, testServer.URL)
			const mode = reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_NON_ROOT
			_, err = host.NegotiateHostProtocol(ctx, connect.NewRequest(&hostv1.NegotiateHostProtocolRequest{AgentId: id, ServiceId: "new-agent.service", StartupId: "new-boot", PrivilegeMode: mode, ProtocolVersion: 2}))
			require.NoError(t, err)
			baseline, err := ordinary.InitializeNodeConfig(ctx, connect.NewRequest(&configv1.InitializeNodeConfigRequest{
				AgentId: id, ServiceId: "new-agent.service", PolicyHash: strings.Repeat("a", 64),
				Privileged: &configv1.PrivilegedConfig{RequiredPrivilegeMode: mode}, Runtime: &configv1.RuntimeConfig{},
			}))
			require.NoError(t, err)
			require.False(t, baseline.Msg.Initialized)

			privileged := configv1connect.NewPrivilegedDeliveryServiceClient(transport, testServer.URL)
			reply, err := privileged.GetPrivilegedDelivery(ctx, connect.NewRequest(&configv1.GetPrivilegedDeliveryRequest{AgentId: id}))
			require.NoError(t, err)
			rev := reply.Msg.Revision
			require.Equal(t, oldRevision+1, rev.Revision)
			require.Equal(t, configv1.PrivilegedDeliveryState_PRIVILEGED_DELIVERY_STATE_AWAITING_AGENT, rev.State)
			require.Equal(t, "new-agent.service", rev.Authorization.GetServiceId())
			require.Empty(t, rev.Authorization.GetActions())
			require.NotEmpty(t, rev.Authorization.GetNonce())
			require.Equal(t, mode, rev.Privileged.RequiredPrivilegeMode)
			require.False(t, rev.Privileged.GetWebsshEnabled())
			require.False(t, rev.Privileged.GetExecutionEnabled())
			require.False(t, rev.Privileged.GetRescueHelperEnabled())
			_, err = privileged.ReportPrivilegedDelivery(ctx, connect.NewRequest(&configv1.ReportPrivilegedDeliveryRequest{
				AgentId: id, Revision: rev.Revision, ProtocolVersion: 2, ReceiptId: uuid.NewString(),
				State: configv1.PrivilegedDeliveryState_PRIVILEGED_DELIVERY_STATE_DELIVERED,
				Observation: &hostv1.DaemonObservation{Revision: rev.Revision, ConfigHash: rev.ConfigHash, ApprovalGeneration: rev.ApprovalGeneration,
					ServiceId: "new-agent.service", StartupId: "new-boot", PrivilegeMode: mode,
					EffectiveCapabilities: &reportv1.AgentCapabilities{PrivilegeMode: mode}, ObservedAt: timestamppb.Now()},
			}))
			require.NoError(t, err)

			record, err := enrollment.GetCredential(id)
			require.NoError(t, err)
			fingerprint := sha256.Sum256(key.Public().(ed25519.PublicKey))
			require.NoError(t, enrollment.ReapproveCredential(ctx, id, record.Generation, base64.RawURLEncoding.EncodeToString(fingerprint[:]), operator))
			_, err = service.RefreshCredentials(ctx, refreshRequest(t, record, creds.RefreshToken, uuid.NewString(), nil, key))
			require.NoError(t, err)
			state, err = privilegeddelivery.GetState(id)
			require.NoError(t, err)
			require.Equal(t, rev.Revision, state.Applied.Revision)
			require.Equal(t, rev.Revision, state.Desired.Revision)
			require.Equal(t, privilegeddelivery.Settings{RequiredPrivilegeMode: int32(mode)}, state.Applied.Settings)
		})
	}
}
