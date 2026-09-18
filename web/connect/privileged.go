package connectapi

// Privileged delivery service.
//
// This is the second delivery track. The ordinary one is a convergence loop:
// the panel names a desired state and the Agent reaches it unattended. These
// settings cannot work that way, because adopting one widens what the Agent is
// permitted to do, and an Agent that could widen its own privileges on the
// panel's word alone would make the panel a single point from which every host
// is escalated.
//
// So nothing here applies anything. The service records an intention, tracks a
// human decision, and issues a single-use nonce that only an upgrade actually
// run on the host can redeem.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/pkg/rpc"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	configv1connect "github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type privilegedDeliveryService struct {
	configv1connect.UnimplementedPrivilegedDeliveryServiceHandler
}

const privilegedWatchHeartbeat = 20 * time.Second

func (s *privilegedDeliveryService) GetPrivilegedDelivery(ctx context.Context, req *connect.Request[configv1.GetPrivilegedDeliveryRequest]) (*connect.Response[configv1.GetPrivilegedDeliveryResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.GetAgentId())
	if err != nil {
		return nil, err
	}
	revision, found, err := privilegeddelivery.Latest(agentID, req.Msg.GetAppliedRevision())
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	response := &configv1.GetPrivilegedDeliveryResponse{}
	if found {
		response.Revision = privilegedRevisionToProto(revision)
	}
	return connect.NewResponse(response), nil
}

func (s *privilegedDeliveryService) WatchPrivilegedDelivery(ctx context.Context, req *connect.Request[configv1.WatchPrivilegedDeliveryRequest], stream *connect.ServerStream[configv1.WatchPrivilegedDeliveryResponse]) error {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.GetAgentId())
	if err != nil {
		return err
	}
	after := req.Msg.GetAfterRevision()
	for {
		revision, found, err := privilegeddelivery.Latest(agentID, after)
		if err != nil {
			return connectError(connect.CodeInternal, err)
		}
		if found {
			if err := stream.Send(&configv1.WatchPrivilegedDeliveryResponse{
				Revision: privilegedRevisionToProto(revision),
			}); err != nil {
				return err
			}
			after = revision.Revision
		}
		// Polling rather than signalling: a privileged revision is created by a
		// human at human cadence, so the simplicity is worth more here than the
		// latency a signal would save.
		timer := time.NewTimer(privilegedWatchHeartbeat)
		select {
		case <-ctx.Done():
			timer.Stop()
			return connectError(connect.CodeCanceled, ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *privilegedDeliveryService) UpdatePrivilegedDelivery(ctx context.Context, req *connect.Request[configv1.UpdatePrivilegedDeliveryRequest]) (*connect.Response[configv1.UpdatePrivilegedDeliveryResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := requireRescueAdministrator(meta); err != nil {
		return nil, err
	}
	agentID := strings.TrimSpace(req.Msg.GetAgentId())
	if agentID == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("an agent ID is required"))
	}
	settings := req.Msg.GetPrivileged()
	if settings == nil {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("privileged settings are required"))
	}

	revision, err := privilegeddelivery.Save(agentID, privilegeddelivery.Settings{
		RemoteControlEnabled:  settings.GetRemoteControlEnabled(),
		WebSSHEnabled:         settings.GetWebsshEnabled(),
		ExecutionEnabled:      settings.GetExecutionEnabled(),
		EnableGPU:             settings.GetEnableGpu(),
		RescueHelperEnabled:   settings.GetRescueHelperEnabled(),
		RequiredPrivilegeMode: int32(settings.GetRequiredPrivilegeMode()),
	}, req.Msg.GetExpectedRevision(), req.Msg.GetReason())
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}

	auditlog.Log(meta.RemoteIP, meta.Principal.UserUUID,
		fmt.Sprintf("privileged configuration delivered: client=%s revision=%d class=%s reason=%s",
			agentID, revision.Revision, configv1.UpgradeClass(revision.UpgradeClass), req.Msg.GetReason()), "warn")

	return connect.NewResponse(&configv1.UpdatePrivilegedDeliveryResponse{
		Revision: privilegedRevisionToProto(revision),
	}), nil
}

func (s *privilegedDeliveryService) ConfirmPrivilegedDelivery(ctx context.Context, req *connect.Request[configv1.ConfirmPrivilegedDeliveryRequest]) (*connect.Response[configv1.ConfirmPrivilegedDeliveryResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := requireRescueAdministrator(meta); err != nil {
		return nil, err
	}
	// Confirming is what authorizes a privilege change on a host, so it always
	// costs a fresh second factor regardless of what the change contains.
	if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	revision, err := privilegeddelivery.Confirm(req.Msg.GetAgentId(), req.Msg.GetRevision(), meta.Principal.UserUUID)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	auditlog.Log(meta.RemoteIP, meta.Principal.UserUUID,
		fmt.Sprintf("privileged configuration confirmed: client=%s revision=%d state=%s",
			req.Msg.GetAgentId(), revision.Revision, configv1.PrivilegedDeliveryState(revision.State)), "warn")
	return connect.NewResponse(&configv1.ConfirmPrivilegedDeliveryResponse{
		Revision: privilegedRevisionToProto(revision),
	}), nil
}

func (s *privilegedDeliveryService) ReportPrivilegedDelivery(ctx context.Context, req *connect.Request[configv1.ReportPrivilegedDeliveryRequest]) (*connect.Response[configv1.ReportPrivilegedDeliveryResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.GetAgentId())
	if err != nil {
		return nil, err
	}
	detail := ""
	if errs := req.Msg.GetErrors(); len(errs) > 0 {
		detail = errs[0].GetMessage()
	}
	revision, err := privilegeddelivery.Report(agentID, req.Msg.GetRevision(), int32(req.Msg.GetState()),
		int32(req.Msg.GetActivePrivilegeMode()), detail)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	// A rollback is reported by the host after the fact, not requested, so it
	// is recorded at a severity an operator will actually notice.
	severity := "info"
	switch req.Msg.GetState() {
	case configv1.PrivilegedDeliveryState_PRIVILEGED_DELIVERY_STATE_ROLLED_BACK,
		configv1.PrivilegedDeliveryState_PRIVILEGED_DELIVERY_STATE_FAILED:
		severity = "warn"
	}
	auditlog.Log("", "", fmt.Sprintf("privileged configuration reported: client=%s revision=%d state=%s mode=%s %s",
		agentID, req.Msg.GetRevision(), req.Msg.GetState(), req.Msg.GetActivePrivilegeMode(), detail), severity)
	return connect.NewResponse(&configv1.ReportPrivilegedDeliveryResponse{
		Accepted: true, Revision: privilegedRevisionToProto(revision),
	}), nil
}

func (s *privilegedDeliveryService) CompleteManualUpgrade(ctx context.Context, req *connect.Request[configv1.CompleteManualUpgradeRequest]) (*connect.Response[configv1.CompleteManualUpgradeResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.GetAgentId())
	if err != nil {
		return nil, err
	}
	revision, err := privilegeddelivery.RedeemNonce(agentID, req.Msg.GetNonce(),
		req.Msg.GetOperator(), req.Msg.GetLocalAuthentication(), int32(req.Msg.GetResultingPrivilegeMode()))
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	auditlog.Log("", "", fmt.Sprintf(
		"privileged upgrade completed on host: client=%s revision=%d operator=%s verified_by=%s mode=%s",
		agentID, revision.Revision, req.Msg.GetOperator(), req.Msg.GetLocalAuthentication(),
		req.Msg.GetResultingPrivilegeMode()), "warn")
	return connect.NewResponse(&configv1.CompleteManualUpgradeResponse{
		Accepted: true, Revision: privilegedRevisionToProto(revision),
	}), nil
}

func privilegedRevisionToProto(revision privilegeddelivery.Revision) *configv1.PrivilegedRevision {
	result := &configv1.PrivilegedRevision{
		AgentId:  revision.Client,
		Revision: revision.Revision,
		Privileged: &configv1.PrivilegedConfig{
			RemoteControlEnabled:  &revision.Settings.RemoteControlEnabled,
			WebsshEnabled:         &revision.Settings.WebSSHEnabled,
			ExecutionEnabled:      &revision.Settings.ExecutionEnabled,
			EnableGpu:             &revision.Settings.EnableGPU,
			RescueHelperEnabled:   &revision.Settings.RescueHelperEnabled,
			RequiredPrivilegeMode: reportv1.PrivilegeMode(revision.Settings.RequiredPrivilegeMode),
		},
		Plan: &configv1.UpgradePlan{
			UpgradeClass:      configv1.UpgradeClass(revision.UpgradeClass),
			Reasons:           revision.Reasons,
			FromPrivilegeMode: reportv1.PrivilegeMode(revision.FromPrivilegeMode),
			ToPrivilegeMode:   reportv1.PrivilegeMode(revision.ToPrivilegeMode),
		},
		State:   configv1.PrivilegedDeliveryState(revision.State),
		SavedAt: timestamppb.New(revision.SavedAt),
	}
	// The manual task travels only while it can still be redeemed. Sending a
	// spent or expired nonce would invite an Agent to attempt an upgrade that
	// cannot succeed.
	if revision.Nonce != "" && revision.NonceUsedAt == nil {
		task := &configv1.ManualUpgradeTask{
			TaskId:               revision.TaskID,
			Nonce:                revision.Nonce,
			Command:              revision.UpgradeCommand,
			RequireLocalPassword: true,
			AuditFacility:        "authpriv",
		}
		if revision.NonceExpires != nil {
			task.ExpiresAt = timestamppb.New(*revision.NonceExpires)
		}
		result.Plan.ManualTask = task
	}
	if revision.ConfirmedAt != nil {
		result.ConfirmedAt = timestamppb.New(*revision.ConfirmedAt)
	}
	if revision.FinishedAt != nil {
		result.FinishedAt = timestamppb.New(*revision.FinishedAt)
	}
	if revision.PreviousRevision != 0 {
		previous := revision.PreviousRevision
		result.PreviousRevision = &previous
	}
	if revision.ErrorDetail != "" {
		result.Errors = []*commonv1.ErrorDetail{{Code: "PRIVILEGED_DELIVERY", Message: revision.ErrorDetail}}
	}
	return result
}
