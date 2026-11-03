package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/internal/credentialaction"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	pluginapp "github.com/komari-monitor/komari/internal/plugin"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	pluginv1 "github.com/r11234567/komari-proto/gen/go/komari/plugin/v1"
)

func pluginHuman(ctx context.Context) (string, error) {
	meta := rpc.MetaFromContext(ctx)
	user, err := requireAdminUser(meta)
	if err != nil {
		return "", err
	}
	if meta.SessionToken == "" || meta.Principal.IsAPIKey || !meta.Principal.HasRole(rpc.RoleAdmin) {
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("interactive administrator session required"))
	}
	return user, nil
}

func (s *pluginService) ListPluginPackages(ctx context.Context, _ *connect.Request[pluginv1.ListPluginPackagesRequest]) (*connect.Response[pluginv1.ListPluginPackagesResponse], error) {
	if _, err := pluginHuman(ctx); err != nil {
		return nil, err
	}
	rows, err := pluginapp.ListPackages()
	if err != nil {
		return nil, pluginGatewayConnectError(err)
	}
	response := &pluginv1.ListPluginPackagesResponse{}
	for _, row := range rows {
		item := &pluginv1.PluginPackage{Plugin: pluginInfoToProto(pluginapp.Info{Plugin: row.Plugin}), Digest: row.Digest, Source: row.Source, PermissionsHash: row.PermissionsHash, CreatedAtUnix: row.CreatedAt.Unix(), ApprovedBy: row.ApprovedBy, Revoked: row.Revoked, Status: row.Status, CurrentDigest: row.CurrentDigest}
		if row.ApprovedAt != nil {
			item.ApprovedAtUnix = row.ApprovedAt.Unix()
		}
		response.Packages = append(response.Packages, item)
	}
	result := connect.NewResponse(response)
	result.Header().Set("Cache-Control", "no-store")
	return result, nil
}

func (s *pluginService) DecidePluginPackage(ctx context.Context, req *connect.Request[pluginv1.DecidePluginPackageRequest]) (*connect.Response[pluginv1.DecidePluginPackageResponse], error) {
	owner, err := pluginHuman(ctx)
	if err != nil {
		return nil, err
	}
	// No nil-proof or "2FA not configured" success path. The ceremony must
	// produce a native, fresh, single-use authorization for this exact package.
	token := req.Msg.TwoFactor.GetAuthorizationToken()
	if token == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("fresh scoped 2FA authorization is required; configure a trusted credential first"))
	}
	err = pluginapp.DecidePackage(req.Msg.ShortName, req.Msg.Digest, req.Msg.ExpectedCurrentDigest, req.Msg.Action, owner, func(review pluginapp.PackageReview) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := credentialgateway.Default.Consume(ctx, token, credential.SensitiveOperation, credentialaction.ScopedResource("plugin-package:"+req.Msg.Action, review.Targets()...))
		if err != nil {
			return credentialConnectError(err)
		}
		// Persist an audit of the authorization attempt before code can execute;
		// package state also retains the approving user/time for successful grants.
		auditlog.Log(rpc.MetaFromContext(ctx).RemoteIP, owner, "authorize plugin package:"+req.Msg.Action+":"+review.Plugin.Short+":"+review.Digest+" previous:"+review.CurrentDigest, "warn")
		return nil
	})
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return nil, ce
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	auditlog.Log(rpc.MetaFromContext(ctx).RemoteIP, owner, "complete plugin package:"+req.Msg.Action+":"+req.Msg.ShortName+":"+req.Msg.Digest, "info")
	return connect.NewResponse(&pluginv1.DecidePluginPackageResponse{}), nil
}
