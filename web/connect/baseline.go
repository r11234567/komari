package connectapi

import (
	"connectrpc.com/connect"
	"context"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/pkg/rpc"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
)

func (s *configService) InitializeNodeConfig(ctx context.Context, req *connect.Request[configv1.InitializeNodeConfigRequest]) (*connect.Response[configv1.InitializeNodeConfigResponse], error) {
	id, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	req.Msg.AgentId = id
	profile := clients.DefaultDeploymentProfile(models.Client{})
	if err = applyRuntime(&profile, req.Msg.Runtime); err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	result, err := privilegeddelivery.InitializeBaseline(dbcore.GetDBInstance(), req.Msg, profile)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	response := &configv1.InitializeNodeConfigResponse{Initialized: result.Initialized, Runtime: result.Runtime, SkippedReason: result.SkippedReason}
	if result.Initialized {
		response.Privileged, err = signedPrivilegedRevision(result.Privileged)
		if err != nil {
			return nil, connectError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(response), nil
}
