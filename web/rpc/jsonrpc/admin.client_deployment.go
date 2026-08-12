package jsonrpc

import (
	"context"
	"errors"

	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	deploymentapp "github.com/komari-monitor/komari/web/deployment"
	"gorm.io/gorm"
)

func init() {
	RegisterWithGroupAndMeta("getClientDeploymentProfile", rpc.RoleAdmin, adminGetClientDeploymentProfile, &rpc.MethodMeta{
		Name:    "admin:getClientDeploymentProfile",
		Summary: "Get a client's saved deployment profile",
		Params: []rpc.ParamMeta{
			{Name: "uuid", Type: "string", Required: true, Description: "Client UUID"},
		},
		Returns: "{ profile: DeploymentProfile, saved: boolean }",
	})
	RegisterWithGroupAndMeta("saveClientDeploymentProfile", rpc.RoleAdmin, adminSaveClientDeploymentProfile, &rpc.MethodMeta{
		Name:    "admin:saveClientDeploymentProfile",
		Summary: "Save a client's deployment profile and dispatch runtime-safe settings",
		Params: []rpc.ParamMeta{
			{Name: "uuid", Type: "string", Required: true, Description: "Client UUID"},
			{Name: "profile", Type: "DeploymentProfile", Required: true},
		},
		Returns: "{ profile: DeploymentProfile, delivery: string }",
	})
}

func adminGetClientDeploymentProfile(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		UUID string `json:"uuid"`
	}
	if err := req.BindParams(&params); err != nil || params.UUID == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid or missing UUID", nil)
	}
	profile, saved, deliveryState, err := clients.GetDeploymentProfileWithDelivery(params.UUID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, rpc.MakeError(rpc.InvalidParams, "Client not found", nil)
		}
		return nil, rpc.MakeError(rpc.InternalError, "Failed to load deployment profile: "+err.Error(), nil)
	}
	return map[string]any{"profile": profile, "saved": saved, "delivery_state": deliveryState}, nil
}

func adminSaveClientDeploymentProfile(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		UUID          string                    `json:"uuid"`
		Profile       clients.DeploymentProfile `json:"profile"`
		ForceDispatch bool                      `json:"force_dispatch"`
	}
	if err := req.BindParams(&params); err != nil || params.UUID == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid deployment profile", nil)
	}
	result, err := deploymentapp.Save(params.UUID, params.Profile, params.ForceDispatch)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, rpc.MakeError(rpc.InvalidParams, "Client not found", nil)
		}
		return nil, rpc.MakeError(rpc.InvalidParams, err.Error(), nil)
	}

	actor, ip := auditActor(ctx)
	auditlog.Log(ip, actor, "save client deployment profile:"+params.UUID, "info")
	return map[string]any{
		"profile":         result.Profile,
		"delivery":        result.Delivery.Status,
		"delivery_state":  result.Delivery,
		"runtime_changed": result.RuntimeChanged,
	}, nil
}
