package jsonrpc

import (
	"context"
	"errors"
	"strings"

	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/internal/plugingateway"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func init() {
	RegisterWithGroupAndMeta("listPlugins", rpc.RoleAdmin, adminListPlugins, &rpc.MethodMeta{
		Name:    "admin:listPlugins",
		Summary: "List installed plugins with enabled/running state",
		Returns: "Plugin[]",
	})
	RegisterWithGroupAndMeta("setPluginEnabled", rpc.RoleAdmin, adminSetPluginEnabled, &rpc.MethodMeta{
		Name:    "admin:setPluginEnabled",
		Summary: "Enable or disable a plugin by short name",
		Returns: "null | { requires_approval: true }",
	})
	RegisterWithGroupAndMeta("getPluginLogs", rpc.RoleAdmin, adminGetPluginLogs, &rpc.MethodMeta{
		Name:    "admin:getPluginLogs",
		Summary: "Get the bounded runtime log buffer of a plugin",
		Returns: "{ logs: string }",
	})
	RegisterWithGroupAndMeta("deletePlugin", rpc.RoleAdmin, adminDeletePlugin, &rpc.MethodMeta{
		Name:    "admin:deletePlugin",
		Summary: "Delete an installed plugin and its persisted state",
		Returns: "null",
	})
	RegisterWithGroupAndMeta("getPluginConfiguration", rpc.RoleAdmin, adminGetPluginConfiguration, &rpc.MethodMeta{
		Name:    "admin:getPluginConfiguration",
		Summary: "Get a plugin's declared config items and saved values",
		Returns: "{ configuration: object, data: object }",
	})
	RegisterWithGroupAndMeta("setPluginConfiguration", rpc.RoleAdmin, adminSetPluginConfiguration, &rpc.MethodMeta{
		Name:    "admin:setPluginConfiguration",
		Summary: "Save a plugin's configuration values",
		Returns: "null",
	})
}

func adminListPlugins(ctx context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	items, err := plugingateway.List(ctx)
	return items, pluginGatewayRPCError(err)
}

// adminSetPluginEnabled is the single switch entry point. When a plugin's
// declared permissions differ from the approved hash, enabling it returns
// { requires_approval: true } instead of an error; the caller then shows a
// permission dialog and retries with approved=true.
func adminSetPluginEnabled(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	short, ok := rpc.GetParamAs[string](req, "short")
	if !ok || strings.TrimSpace(short) == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "short is required", nil)
	}
	enabled, _ := rpc.GetParamAs[bool](req, "enabled")
	approved, _ := rpc.GetParamAs[bool](req, "approved")
	if err := plugingateway.SetEnabled(ctx, short, enabled, approved); err != nil {
		if errors.Is(err, plugin.ErrPermissionApprovalRequired) {
			return map[string]any{"requires_approval": true}, nil
		}
		return nil, pluginGatewayRPCError(err)
	}
	return nil, nil
}

func adminGetPluginLogs(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	short, ok := rpc.GetParamAs[string](req, "short")
	if !ok || strings.TrimSpace(short) == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "short is required", nil)
	}
	logs, err := plugingateway.Logs(ctx, short)
	return map[string]any{"logs": logs}, pluginGatewayRPCError(err)
}

func adminDeletePlugin(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	short, ok := rpc.GetParamAs[string](req, "short")
	if !ok || strings.TrimSpace(short) == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "short is required", nil)
	}
	if err := plugingateway.Delete(ctx, short); err != nil {
		if errors.Is(err, plugin.ErrNotInstalled) {
			return nil, rpc.MakeError(rpc.NotFound, err.Error(), nil)
		}
		return nil, pluginGatewayRPCError(err)
	}
	return nil, nil
}

func adminGetPluginConfiguration(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	short, ok := rpc.GetParamAs[string](req, "short")
	if !ok || strings.TrimSpace(short) == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "short is required", nil)
	}
	configuration, values, err := plugingateway.Configuration(ctx, short)
	if err != nil {
		return nil, pluginGatewayRPCError(err)
	}
	return map[string]any{"configuration": configuration, "data": values}, nil
}

func adminSetPluginConfiguration(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	short, ok := rpc.GetParamAs[string](req, "short")
	if !ok || strings.TrimSpace(short) == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "short is required", nil)
	}
	data, _ := rpc.GetParamAs[map[string]any](req, "data")
	if err := plugingateway.SaveConfiguration(ctx, short, data); err != nil {
		return nil, pluginGatewayRPCError(err)
	}
	return nil, nil
}

func pluginGatewayRPCError(err error) *rpc.JsonRpcError {
	if err == nil {
		return nil
	}
	code := rpc.InternalError
	switch {
	case errors.Is(err, plugingateway.ErrPermissionDenied):
		code = rpc.PermissionDenied
	case errors.Is(err, plugingateway.ErrShortRequired):
		code = rpc.InvalidParams
	case errors.Is(err, plugin.ErrNotInstalled), errors.Is(err, plugingateway.ErrManifestLookup):
		code = rpc.NotFound
	case errors.Is(err, context.Canceled):
		code = rpc.Cancelled
	case errors.Is(err, context.DeadlineExceeded):
		code = rpc.DeadlineExceeded
	}
	return rpc.MakeError(code, err.Error(), nil)
}
