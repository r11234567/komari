package connectapi

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	"github.com/komari-monitor/komari/web/executionapp"
	execv1 "github.com/r11234567/komari-proto/gen/go/komari/exec/v1"
	execv1connect "github.com/r11234567/komari-proto/gen/go/komari/exec/v1/execv1connect"
)

type executionService struct {
	execv1connect.UnimplementedExecutionServiceHandler
}

func (s *executionService) CreateExecution(ctx context.Context, req *connect.Request[execv1.CreateExecutionRequest]) (*connect.Response[execv1.CreateExecutionResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	ownerID, err := requireAdminUser(meta)
	if err != nil {
		return nil, err
	}
	if err := verifyConnectTwoFactor(meta, req.Msg.TwoFactor); err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	agentIDs := append([]string(nil), req.Msg.AgentIds...)
	if strings.TrimSpace(req.Msg.AgentId) != "" {
		if len(agentIDs) > 0 {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("agent_id and agent_ids cannot be combined"))
		}
		agentIDs = []string{req.Msg.AgentId}
	}
	if len(agentIDs) == 0 || strings.TrimSpace(req.Msg.Command) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent_id and command are required"))
	}
	seen := make(map[string]struct{}, len(agentIDs))
	for index, value := range agentIDs {
		agentID := strings.TrimSpace(value)
		if agentID == "" {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("agent ID is required"))
		}
		if _, duplicate := seen[agentID]; duplicate {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("agent IDs must be unique"))
		}
		seen[agentID] = struct{}{}
		agentIDs[index] = agentID
		if _, err := clients.GetClientByUUID(agentID); err != nil {
			return nil, connectError(connect.CodeNotFound, errors.New("agent not found: "+agentID))
		}
		allowed, err := clients.RemoteControlAllowed(agentID)
		if err != nil {
			return nil, connectError(connect.CodeInternal, err)
		}
		if !allowed {
			return nil, connectError(connect.CodePermissionDenied, errors.New("remote control is disabled for agent: "+agentID))
		}
		if !agentRuntime.IsAgentOnline(agentID) || !agentRuntime.SupportsExecution(agentID) {
			return nil, connectError(connect.CodeFailedPrecondition, errors.New("agent is offline or does not support Connect execution: "+agentID))
		}
	}
	executions, err := executionapp.CreateBatch(agentIDs, ownerID, req.Msg.IdempotencyKey, &execv1.ExecutionSpec{
		Command: req.Msg.Command, Arguments: req.Msg.Arguments, Environment: req.Msg.Environment,
		WorkingDirectory: req.Msg.WorkingDirectory, Timeout: req.Msg.Timeout, MaxOutputBytes: req.Msg.MaxOutputBytes,
	})
	if err != nil {
		return nil, executionError(err)
	}
	for _, execution := range executions {
		auditlog.Log(meta.RemoteIP, ownerID, "create Connect execution:"+execution.ExecutionId+", client:"+execution.AgentId, "warn")
	}
	return connect.NewResponse(&execv1.CreateExecutionResponse{Execution: executions[0], Executions: executions}), nil
}

func (s *executionService) WatchExecution(ctx context.Context, req *connect.Request[execv1.WatchExecutionRequest], stream *connect.ServerStream[execv1.WatchExecutionResponse]) error {
	ownerID, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return err
	}
	task, err := executionapp.GetOwned(req.Msg.ExecutionId, ownerID)
	if err != nil {
		return executionError(err)
	}
	sequence := req.Msg.AfterSequence
	for {
		event, err := task.NextEvent(ctx, sequence)
		if errors.Is(err, executionapp.ErrTerminal) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&execv1.WatchExecutionResponse{Event: event}); err != nil {
			return err
		}
		sequence = event.Sequence
	}
}

func (s *executionService) CancelExecution(ctx context.Context, req *connect.Request[execv1.CancelExecutionRequest]) (*connect.Response[execv1.CancelExecutionResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	ownerID, err := requireAdminUser(meta)
	if err != nil {
		return nil, err
	}
	if err := verifyConnectTwoFactor(meta, req.Msg.TwoFactor); err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	if _, err := executionapp.GetOwned(req.Msg.ExecutionId, ownerID); err != nil {
		return nil, executionError(err)
	}
	execution, err := executionapp.Cancel(req.Msg.ExecutionId, req.Msg.Reason)
	if err != nil {
		return nil, executionError(err)
	}
	auditlog.Log(meta.RemoteIP, ownerID, "cancel Connect execution:"+execution.ExecutionId+", client:"+execution.AgentId, "warn")
	return connect.NewResponse(&execv1.CancelExecutionResponse{Execution: execution}), nil
}

func (s *executionService) GetExecution(ctx context.Context, req *connect.Request[execv1.GetExecutionRequest]) (*connect.Response[execv1.GetExecutionResponse], error) {
	ownerID, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, err
	}
	task, err := executionapp.GetOwned(req.Msg.ExecutionId, ownerID)
	if err != nil {
		return nil, executionError(err)
	}
	return connect.NewResponse(&execv1.GetExecutionResponse{Execution: task.Snapshot()}), nil
}

func (s *executionService) LeaseExecution(ctx context.Context, req *connect.Request[execv1.LeaseExecutionRequest], stream *connect.ServerStream[execv1.LeaseExecutionResponse]) error {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return err
	}
	agentRuntime.MarkConnectExecutionLease(agentID)
	delivered := make(map[string]bool)
	for {
		dispatch, err := executionapp.NextDispatch(ctx, agentID, req.Msg.AfterAssignmentId, delivered)
		if err != nil {
			return err
		}
		if err := stream.Send(&execv1.LeaseExecutionResponse{Assignment: dispatch.Assignment, Cancellation: dispatch.Cancellation}); err != nil {
			return err
		}
	}
}

func (s *executionService) ReportExecutionEvent(ctx context.Context, req *connect.Request[execv1.ReportExecutionEventRequest]) (*connect.Response[execv1.ReportExecutionEventResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	sequence, err := executionapp.Report(agentID, req.Msg.Event)
	if err != nil {
		return nil, executionError(err)
	}
	return connect.NewResponse(&execv1.ReportExecutionEventResponse{AcceptedSequence: sequence}), nil
}

func executionError(err error) error {
	switch {
	case errors.Is(err, executionapp.ErrNotFound):
		return connectError(connect.CodeNotFound, err)
	case errors.Is(err, executionapp.ErrForbidden):
		return connectError(connect.CodePermissionDenied, err)
	case errors.Is(err, executionapp.ErrInvalid), errors.Is(err, executionapp.ErrTerminal):
		return connectError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, executionapp.ErrOutput), errors.Is(err, executionapp.ErrTaskLimit):
		return connectError(connect.CodeResourceExhausted, err)
	default:
		return connectError(connect.CodeInvalidArgument, err)
	}
}
