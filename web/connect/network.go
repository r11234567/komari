package connectapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/pkg/rpc"
	legacyv2 "github.com/komari-monitor/komari/protocol/v2"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	clientapi "github.com/komari-monitor/komari/web/api/client"
	networkv1 "github.com/r11234567/komari-proto/gen/go/komari/network/v1"
	networkv1connect "github.com/r11234567/komari-proto/gen/go/komari/network/v1/networkv1connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const networkProbeLongPoll = 25 * time.Second

type networkProbeService struct {
	networkv1connect.UnimplementedNetworkProbeServiceHandler
}

func (s *networkProbeService) LeasePingProbe(ctx context.Context, req *connect.Request[networkv1.LeasePingProbeRequest]) (*connect.Response[networkv1.LeasePingProbeResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	// Ping leases are continuously renewed by the primary Agent even when no
	// task is queued. They provide a transport-independent presence heartbeat
	// for older Connect Agents whose metric stream may be stalled by a proxy.
	clientapi.TouchConnectPresence(agentID)
	agentRuntime.MarkConnectPingLease(agentID)
	waitCtx, cancel := context.WithTimeout(ctx, networkProbeLongPoll)
	defer cancel()
	assignment, err := agentRuntime.WaitPingProbe(waitCtx, agentID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return connect.NewResponse(&networkv1.LeasePingProbeResponse{}), nil
		}
		return nil, err
	}
	return connect.NewResponse(&networkv1.LeasePingProbeResponse{Assignment: &networkv1.PingProbeAssignment{
		AssignmentId: assignment.AssignmentID, TaskId: uint64(assignment.TaskID), Protocol: assignment.Protocol,
		Target: assignment.Target, Timeout: durationpb.New(3 * time.Second), LeaseExpiresAt: timestamppb.New(assignment.LeaseExpires),
	}}), nil
}

func (s *networkProbeService) SubmitPingProbeResult(ctx context.Context, req *connect.Request[networkv1.SubmitPingProbeResultRequest]) (*connect.Response[networkv1.SubmitPingProbeResultResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if req.Msg.TaskId == 0 || strings.TrimSpace(req.Msg.AssignmentId) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("assignment_id and task_id are required"))
	}
	taskID := uint(req.Msg.TaskId)
	maxInt := int64(^uint(0) >> 1)
	if uint64(taskID) != req.Msg.TaskId || req.Msg.LatencyMs < -1 || req.Msg.LatencyMs > maxInt {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("ping result is outside the supported range"))
	}
	alreadyCompleted, err := agentRuntime.ValidatePingProbeResult(agentID, req.Msg.AssignmentId, taskID)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	if alreadyCompleted {
		return connect.NewResponse(&networkv1.SubmitPingProbeResultResponse{Accepted: true}), nil
	}
	finishedAt := time.Now().UTC()
	if req.Msg.FinishedAt != nil {
		if !req.Msg.FinishedAt.IsValid() {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("finished_at is invalid"))
		}
		finishedAt = req.Msg.FinishedAt.AsTime().UTC()
	}
	if err := tasks.SavePingRecordContext(ctx, models.PingRecord{Client: agentID, TaskId: taskID, Time: finishedAt, Value: int(req.Msg.LatencyMs)}); err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	agentRuntime.CompletePingProbe(agentID, req.Msg.AssignmentId, taskID)
	return connect.NewResponse(&networkv1.SubmitPingProbeResultResponse{Accepted: true}), nil
}

func (s *networkProbeService) LeaseReturnRouteProbe(ctx context.Context, req *connect.Request[networkv1.LeaseReturnRouteProbeRequest]) (*connect.Response[networkv1.LeaseReturnRouteProbeResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	agentRuntime.MarkConnectReturnRouteLease(agentID)
	waitCtx, cancel := context.WithTimeout(ctx, networkProbeLongPoll)
	defer cancel()
	assignment, err := agentRuntime.WaitReturnRouteProbe(waitCtx, agentID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return connect.NewResponse(&networkv1.LeaseReturnRouteProbeResponse{}), nil
		}
		return nil, err
	}
	return connect.NewResponse(&networkv1.LeaseReturnRouteProbeResponse{Assignment: &networkv1.ReturnRouteProbeAssignment{
		AssignmentId: assignment.AssignmentID, TaskId: uint64(assignment.TaskID), Protocol: assignment.Protocol,
		Target: assignment.Target, IpVersion: uint32(assignment.IPVersion), MaxHops: uint32(assignment.MaxHops),
		HopTimeout: durationpb.New(900 * time.Millisecond), LeaseExpiresAt: timestamppb.New(assignment.LeaseExpires),
	}}), nil
}

func (s *networkProbeService) SubmitReturnRouteProbeResult(ctx context.Context, req *connect.Request[networkv1.SubmitReturnRouteProbeResultRequest]) (*connect.Response[networkv1.SubmitReturnRouteProbeResultResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if req.Msg.TaskId == 0 || req.Msg.AssignmentId == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("assignment_id and task_id are required"))
	}
	taskID := uint(req.Msg.TaskId)
	if uint64(taskID) != req.Msg.TaskId {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("task_id exceeds the platform integer range"))
	}
	alreadyCompleted, err := agentRuntime.ValidateReturnRouteResult(agentID, req.Msg.AssignmentId, taskID)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	if alreadyCompleted {
		return connect.NewResponse(&networkv1.SubmitReturnRouteProbeResultResponse{Accepted: true}), nil
	}
	result := legacyv2.RouteResultParams{TaskID: taskID, Error: req.Msg.Error}
	if req.Msg.FinishedAt != nil && req.Msg.FinishedAt.IsValid() {
		result.FinishedAt = req.Msg.FinishedAt.AsTime()
	}
	result.Hops = make([]legacyv2.RouteHop, 0, len(req.Msg.Hops))
	for _, hop := range req.Msg.Hops {
		if hop != nil {
			result.Hops = append(result.Hops, legacyv2.RouteHop{TTL: int(hop.Ttl), IP: hop.Ip, LatencyMS: hop.LatencyMs, Timeout: hop.Timeout})
		}
	}
	if err := tasks.SaveReturnRouteResult(agentID, result); err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	agentRuntime.CompleteReturnRouteProbe(agentID, req.Msg.AssignmentId, taskID)
	return connect.NewResponse(&networkv1.SubmitReturnRouteProbeResultResponse{Accepted: true}), nil
}
