package connectapi

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/rescueapp"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	rescuev1 "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1"
	rescuev1connect "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1/rescuev1connect"
	"gorm.io/gorm"
)

type rescueService struct {
	rescuev1connect.UnimplementedRescueServiceHandler
}

const rescueLeaseHeartbeatInterval = 20 * time.Second

func (s *rescueService) GetRescueStatus(_ context.Context, req *connect.Request[rescuev1.GetRescueStatusRequest]) (*connect.Response[rescuev1.GetRescueStatusResponse], error) {
	if req.Msg.AgentId == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent ID is required"))
	}
	status, err := rescueapp.GetStatus(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&rescuev1.GetRescueStatusResponse{Status: status}), nil
}

func (s *rescueService) CreateRescueSession(ctx context.Context, req *connect.Request[rescuev1.CreateRescueSessionRequest]) (*connect.Response[rescuev1.CreateRescueSessionResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.TwoFactor); err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	session, err := rescueapp.Create(req.Msg.AgentId, req.Msg.Action, req.Msg.Arguments, req.Msg.Timeout, req.Msg.MaxOutputBytes, req.Msg.IdempotencyKey)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	auditlog.Log(meta.RemoteIP, meta.Principal.UserUUID, "create rescue session:"+session.SessionId+", client:"+session.AgentId, "warn")
	return connect.NewResponse(&rescuev1.CreateRescueSessionResponse{Session: session}), nil
}

func (s *rescueService) WatchRescueSession(ctx context.Context, req *connect.Request[rescuev1.WatchRescueSessionRequest], stream *connect.ServerStream[rescuev1.WatchRescueSessionResponse]) error {
	if req.Msg.SessionId == "" {
		return connectError(connect.CodeInvalidArgument, errors.New("session ID is required"))
	}
	session, _, err := rescueapp.ExpireIfOverdue(req.Msg.SessionId)
	if err != nil {
		return connectError(connect.CodeNotFound, err)
	}
	if err := stream.Send(&rescuev1.WatchRescueSessionResponse{Session: session}); err != nil {
		return err
	}
	if rescueTerminal(session.State) {
		return nil
	}
	lastState := session.State
	sequence := req.Msg.AfterSequence
	for {
		currentSignal := rescueapp.WaitSignal("session:" + req.Msg.SessionId)
		events, err := rescueapp.EventsAfter(req.Msg.SessionId, sequence)
		if err != nil {
			return connectError(connect.CodeInternal, err)
		}
		for _, event := range events {
			if err := stream.Send(&rescuev1.WatchRescueSessionResponse{Event: event}); err != nil {
				return err
			}
			sequence = event.Sequence
			if rescueTerminal(event.State) {
				return nil
			}
		}
		session, _, err = rescueapp.ExpireIfOverdue(req.Msg.SessionId)
		if err != nil {
			return connectError(connect.CodeInternal, err)
		}
		if session.State != lastState {
			if err := stream.Send(&rescuev1.WatchRescueSessionResponse{Session: session}); err != nil {
				return err
			}
			lastState = session.State
			if rescueTerminal(session.State) {
				return nil
			}
		}
		deadlineWait := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			deadlineWait.Stop()
			return connectError(connect.CodeCanceled, ctx.Err())
		case <-currentSignal:
			deadlineWait.Stop()
		case <-deadlineWait.C:
		}
	}
}

func (s *rescueService) CancelRescueSession(ctx context.Context, req *connect.Request[rescuev1.CancelRescueSessionRequest]) (*connect.Response[rescuev1.CancelRescueSessionResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.TwoFactor); err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	session, err := rescueapp.Cancel(req.Msg.SessionId, req.Msg.Reason)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, err)
	}
	auditlog.Log(meta.RemoteIP, meta.Principal.UserUUID, "cancel rescue session:"+session.SessionId+", client:"+session.AgentId, "warn")
	return connect.NewResponse(&rescuev1.CancelRescueSessionResponse{Session: session}), nil
}

func (s *rescueService) LeaseRescueSessions(ctx context.Context, req *connect.Request[rescuev1.LeaseRescueSessionsRequest], stream *connect.ServerStream[rescuev1.LeaseRescueSessionsResponse]) error {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return err
	}
	if err := rescueapp.ValidateHelper(agentID, req.Msg.HelperInstanceId); err != nil {
		return connectError(connect.CodeFailedPrecondition, err)
	}
	if err := rescueapp.ClearConnectionError(agentID, req.Msg.HelperInstanceId); err != nil {
		return connectError(connect.CodeInternal, err)
	}
	for {
		currentSignal := rescueapp.WaitSignal("lease:" + agentID)
		assignment, err := rescueapp.NextAssignment(agentID, req.Msg.HelperInstanceId, req.Msg.AfterAssignmentId)
		if err == nil {
			return stream.Send(&rescuev1.LeaseRescueSessionsResponse{Assignment: assignment})
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return connectError(connect.CodeInternal, err)
		}
		heartbeat := time.NewTimer(rescueLeaseHeartbeatInterval)
		select {
		case <-ctx.Done():
			heartbeat.Stop()
			return connectError(connect.CodeCanceled, ctx.Err())
		case <-currentSignal:
			heartbeat.Stop()
		case <-heartbeat.C:
			return stream.Send(&rescuev1.LeaseRescueSessionsResponse{})
		}
	}
}

func (s *rescueService) ReportRescueEvent(ctx context.Context, req *connect.Request[rescuev1.ReportRescueEventRequest]) (*connect.Response[rescuev1.ReportRescueEventResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if err := rescueapp.ValidateHelper(agentID, req.Msg.HelperInstanceId); err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	sequence, err := rescueapp.ReportEvent(agentID, req.Msg.HelperInstanceId, req.Msg.Event)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	if req.Msg.Event != nil && rescueTerminal(req.Msg.Event.State) {
		meta := rpc.MetaFromContext(ctx)
		auditlog.Log(meta.RemoteIP, "", "complete rescue session:"+req.Msg.Event.SessionId+", client:"+agentID+", state:"+req.Msg.Event.State.String(), "warn")
	}
	return connect.NewResponse(&rescuev1.ReportRescueEventResponse{AcceptedSequence: sequence}), nil
}

func (s *rescueService) ReportRescueStatus(ctx context.Context, req *connect.Request[rescuev1.ReportRescueStatusRequest]) (*connect.Response[rescuev1.ReportRescueStatusResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if err := rescueapp.ReportStatus(agentID, req.Msg.Status); err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&rescuev1.ReportRescueStatusResponse{Accepted: true}), nil
}

func verifyConnectTwoFactor(meta *rpc.ContextMeta, proof *commonv1.TwoFactorProof) error {
	if meta == nil || meta.Principal == nil {
		return errors.New("administrator authentication is required")
	}
	code := ""
	if proof != nil {
		code = proof.Code
	}
	return api.VerifySensitive2FACore(meta.Principal.UserUUID, code, meta.Principal.IsAPIKey)
}

func rescueTerminal(state commonv1.OperationState) bool {
	return state == commonv1.OperationState_OPERATION_STATE_CANCELLED ||
		state == commonv1.OperationState_OPERATION_STATE_DEADLINE_EXCEEDED ||
		state == commonv1.OperationState_OPERATION_STATE_FAILED ||
		state == commonv1.OperationState_OPERATION_STATE_SUCCEEDED
}
