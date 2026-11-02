package connectapi

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/apitokens"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
	adminv1connect "github.com/r11234567/komari-proto/gen/go/komari/admin/v1/adminv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type apiTokenService struct {
	adminv1connect.UnimplementedApiTokenServiceHandler
}

func apiTokenView(row models.APIToken) *adminv1.ApiToken {
	view := &adminv1.ApiToken{Id: row.ID, Name: row.Name, AllAgents: row.AllAgents, CreatedAt: timestamppb.New(row.CreatedAt), ExpiresAt: timestamppb.New(row.ExpiresAt)}
	_ = json.Unmarshal([]byte(row.Scopes), &view.Scopes)
	_ = json.Unmarshal([]byte(row.AgentIDs), &view.AgentIds)
	_ = json.Unmarshal([]byte(row.AllowedCIDRs), &view.AllowedCidrs)
	if row.LastUsedAt != nil {
		view.LastUsedAt = timestamppb.New(*row.LastUsedAt)
	}
	if row.RevokedAt != nil {
		view.RevokedAt = timestamppb.New(*row.RevokedAt)
	}
	return view
}

func (s *apiTokenService) ListApiTokens(ctx context.Context, req *connect.Request[adminv1.ListApiTokensRequest]) (*connect.Response[adminv1.ListApiTokensResponse], error) {
	owner, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, err
	}
	page := max(req.Msg.Page, 1)
	if page > 100000 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("page is too large"))
	}
	rows, total, err := apitokens.List(ctx, owner, int(page))
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	legacy, err := config.GetAs[string](config.ApiKeyKey, "")
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	response := &adminv1.ListApiTokensResponse{Total: uint32(total), Page: page, LegacyApiKeyEnabled: len(legacy) >= 12}
	for _, row := range rows {
		response.Tokens = append(response.Tokens, apiTokenView(row))
	}
	return connect.NewResponse(response), nil
}

func apiTokenCreateTargets(req *adminv1.CreateApiTokenRequest) []string {
	encode := func(values []string) string {
		if values == nil {
			values = []string{}
		}
		data, _ := json.Marshal(values)
		return string(data)
	}
	return []string{req.Name, encode(req.Scopes), encode(req.AgentIds), strconv.FormatBool(req.AllAgents), strconv.FormatInt(req.ExpiresAt.GetSeconds(), 10) + ":" + strconv.Itoa(int(req.ExpiresAt.GetNanos())), encode(req.AllowedCidrs), req.ReplacesTokenId}
}

func (s *apiTokenService) CreateApiToken(ctx context.Context, req *connect.Request[adminv1.CreateApiTokenRequest]) (*connect.Response[adminv1.CreateApiTokenResponse], error) {
	owner, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, err
	}
	if req.Msg.ExpiresAt == nil || !req.Msg.ExpiresAt.IsValid() {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("an explicit expiry is required"))
	}
	policy, err := apitokens.Validate(apitokens.Policy{Name: req.Msg.Name, Scopes: req.Msg.Scopes, AgentIDs: req.Msg.AgentIds, AllAgents: req.Msg.AllAgents, ExpiresAt: req.Msg.ExpiresAt.AsTime(), AllowedCIDRs: req.Msg.AllowedCidrs}, time.Now().UTC())
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	if err := verifyConnectCredential(ctx, req.Msg.TwoFactor, "api-token:create", apiTokenCreateTargets(req.Msg)...); err != nil {
		return nil, err
	}
	row, secret, err := apitokens.Create(ctx, owner, policy, req.Msg.ReplacesTokenId)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	meta := rpc.MetaFromContext(ctx)
	auditlog.Log(meta.RemoteIP, owner, "create API token:"+row.ID+" replaces:"+req.Msg.ReplacesTokenId, "info")
	response := connect.NewResponse(&adminv1.CreateApiTokenResponse{Token: apiTokenView(row), Secret: secret})
	response.Header().Set("Cache-Control", "no-store")
	return response, nil
}

func (s *apiTokenService) RevokeApiToken(ctx context.Context, req *connect.Request[adminv1.RevokeApiTokenRequest]) (*connect.Response[adminv1.RevokeApiTokenResponse], error) {
	owner, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, err
	}
	if req.Msg.Id == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("token ID is required"))
	}
	if err := verifyConnectCredential(ctx, req.Msg.TwoFactor, "api-token:revoke", req.Msg.Id); err != nil {
		return nil, err
	}
	if err := apitokens.Revoke(ctx, owner, req.Msg.Id); err != nil {
		return nil, connectError(connect.CodeNotFound, err)
	}
	auditlog.Log(rpc.MetaFromContext(ctx).RemoteIP, owner, "revoke API token:"+req.Msg.Id, "info")
	return connect.NewResponse(&adminv1.RevokeApiTokenResponse{}), nil
}

func (s *apiTokenService) DisableLegacyApiKey(ctx context.Context, req *connect.Request[adminv1.DisableLegacyApiKeyRequest]) (*connect.Response[adminv1.DisableLegacyApiKeyResponse], error) {
	owner, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, err
	}
	if err := verifyConnectCredential(ctx, req.Msg.TwoFactor, "api-token:disable-legacy"); err != nil {
		return nil, err
	}
	if err := config.Set(config.ApiKeyKey, ""); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	auditlog.Log(rpc.MetaFromContext(ctx).RemoteIP, owner, "disable legacy global API key", "warn")
	return connect.NewResponse(&adminv1.DisableLegacyApiKeyResponse{}), nil
}
