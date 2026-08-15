package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/agentevents"
	agentv1 "github.com/r11234567/komari-proto/gen/go/komari/agent/v1"
	agentv1connect "github.com/r11234567/komari-proto/gen/go/komari/agent/v1/agentv1connect"
)

type agentEventService struct {
	agentv1connect.UnimplementedAgentEventServiceHandler
}

func (s *agentEventService) PublishEvent(ctx context.Context, req *connect.Request[agentv1.PublishEventRequest]) (*connect.Response[agentv1.PublishEventResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.GetEvent().GetAgentId())
	if err != nil {
		return nil, err
	}
	eventID, err := agentevents.Publish(agentID, req.Msg.Event)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&agentv1.PublishEventResponse{Accepted: true, EventId: eventID}), nil
}

func (s *agentEventService) SubscribeEvents(ctx context.Context, req *connect.Request[agentv1.SubscribeEventsRequest], stream *connect.ServerStream[agentv1.SubscribeEventsResponse]) error {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return err
	}
	after := req.Msg.AfterEventId
	for {
		event, err := agentevents.Next(ctx, agentID, after)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&agentv1.SubscribeEventsResponse{Event: event}); err != nil {
			return err
		}
		after = event.EventId
	}
}

func (s *agentEventService) AcknowledgeEvent(ctx context.Context, req *connect.Request[agentv1.AcknowledgeEventRequest]) (*connect.Response[agentv1.AcknowledgeEventResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if !agentevents.Acknowledge(agentID, req.Msg.EventId) {
		return nil, connectError(connect.CodeNotFound, errors.New("server event not found"))
	}
	return connect.NewResponse(&agentv1.AcknowledgeEventResponse{Accepted: true}), nil
}
