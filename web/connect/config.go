package connectapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/agentevents"
	deploymentapp "github.com/komari-monitor/komari/web/deployment"
	agentv1 "github.com/r11234567/komari-proto/gen/go/komari/agent/v1"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	configv1connect "github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type configService struct {
	configv1connect.UnimplementedConfigServiceHandler
}

func (s *configService) GetDesiredConfig(ctx context.Context, req *connect.Request[configv1.GetDesiredConfigRequest]) (*connect.Response[configv1.GetDesiredConfigResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	profile, saved, state, err := deploymentapp.GetDesired(agentID, req.Msg.AppliedRevision)
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	response := &configv1.GetDesiredConfigResponse{DeliveryState: deliveryState(state.Status)}
	if saved {
		response.Desired = &configv1.DesiredConfig{
			AgentId: agentID, Revision: state.Revision, Runtime: runtimeToProto(profile.RuntimeConfig()),
			SavedAt: timestamppb.New(state.SavedAt),
		}
		_, _ = clients.MarkDeploymentConfigSent(agentID, state.Revision)
	}
	return connect.NewResponse(response), nil
}

func (s *configService) AcknowledgeConfig(ctx context.Context, req *connect.Request[configv1.AcknowledgeConfigRequest]) (*connect.Response[configv1.AcknowledgeConfigResponse], error) {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	status := ""
	switch req.Msg.Status {
	case configv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED:
		status = clients.DeploymentDeliveryApplied
	case configv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED:
		status = clients.DeploymentDeliveryRejected
	case configv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_UPGRADE_REQUIRED:
		status = clients.DeploymentDeliveryUpgradeRequired
	default:
		return nil, connectError(connect.CodeInvalidArgument, errors.New("terminal config status is required"))
	}
	messages := make([]string, 0, len(req.Msg.Errors))
	for _, detail := range req.Msg.Errors {
		if detail != nil && strings.TrimSpace(detail.Message) != "" {
			messages = append(messages, detail.Message)
		}
	}
	accepted, err := clients.CompleteDeploymentConfigDelivery(agentID, req.Msg.Revision, status, strings.Join(messages, "; "))
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	if !accepted {
		return nil, connectError(connect.CodeAborted, errors.New("configuration revision is stale"))
	}
	return connect.NewResponse(&configv1.AcknowledgeConfigResponse{Accepted: true, DeliveryState: deliveryState(status)}), nil
}

func (s *configService) UpdateDesiredConfig(ctx context.Context, req *connect.Request[configv1.UpdateDesiredConfigRequest]) (*connect.Response[configv1.UpdateDesiredConfigResponse], error) {
	profile, _, state, err := clients.GetDeploymentProfileWithDelivery(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	if req.Msg.ExpectedRevision != state.Revision {
		return nil, connectError(connect.CodeAborted, errors.New("configuration revision conflict"))
	}
	if err := applyRuntime(&profile, req.Msg.Runtime); err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	result, err := deploymentapp.Save(req.Msg.AgentId, profile, req.Msg.ForceDispatch)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	agentevents.Notify(req.Msg.AgentId, agentv1.ServerEventType_SERVER_EVENT_TYPE_CONFIG_AVAILABLE, map[string]string{"revision": fmt.Sprintf("%d", result.Delivery.Revision)})
	return connect.NewResponse(&configv1.UpdateDesiredConfigResponse{
		Desired: &configv1.DesiredConfig{
			AgentId: req.Msg.AgentId, Revision: result.Delivery.Revision,
			Runtime: runtimeToProto(result.Profile.RuntimeConfig()), SavedAt: timestamppb.New(result.Delivery.SavedAt),
		},
		DeliveryState: deliveryState(result.Delivery.Status),
	}), nil
}

func (s *configService) WatchDesiredConfig(ctx context.Context, req *connect.Request[configv1.WatchDesiredConfigRequest], stream *connect.ServerStream[configv1.WatchDesiredConfigResponse]) error {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return err
	}
	profile, found, state, err := deploymentapp.WaitForDesired(ctx, agentID, req.Msg.AfterRevision)
	if err != nil {
		return connectError(connect.CodeCanceled, err)
	}
	if !found {
		return nil
	}
	if err := stream.Send(&configv1.WatchDesiredConfigResponse{Desired: &configv1.DesiredConfig{
		AgentId: agentID, Revision: state.Revision, Runtime: runtimeToProto(profile.RuntimeConfig()), SavedAt: timestamppb.New(state.SavedAt),
	}}); err != nil {
		return err
	}
	_, _ = clients.MarkDeploymentConfigSent(agentID, state.Revision)
	return nil
}
