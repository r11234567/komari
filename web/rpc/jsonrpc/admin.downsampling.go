package jsonrpc

import (
	"context"
	"fmt"

	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/records"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func init() {
	reg("getDownsamplingPolicy", adminGetDownsamplingPolicy, "Get the four-tier metric downsampling policy")
	reg("setDownsamplingPolicy", adminSetDownsamplingPolicy, "Update the four-tier metric downsampling policy")
}

func adminGetDownsamplingPolicy(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	policy, err := records.GetDownsamplingPolicy()
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to load downsampling policy: "+err.Error(), nil)
	}
	return policy, nil
}

func adminSetDownsamplingPolicy(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var policy records.DownsamplingPolicy
	if err := req.BindParams(&policy); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid downsampling policy: "+err.Error(), nil)
	}
	if err := records.SetDownsamplingPolicy(policy); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid downsampling policy: "+err.Error(), nil)
	}

	actor, ip := auditActor(ctx)
	auditlog.Log(ip, actor, fmt.Sprintf("updated four-tier downsampling policy (enabled=%t)", policy.Enabled), "warn")
	return policy, nil
}
