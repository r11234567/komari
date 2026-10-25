package connectapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/workspace"
	workspacev1 "github.com/r11234567/komari-proto/gen/go/komari/workspace/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type workspaceService struct{}

func (s *workspaceService) UnlockWorkspace(ctx context.Context, req *connect.Request[workspacev1.UnlockWorkspaceRequest]) (*connect.Response[workspacev1.UnlockWorkspaceResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	user, session, err := workspace.Interactive(meta)
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	agent := strings.TrimSpace(req.Msg.AgentId)
	if agent == "" || req.Msg.SystemUser == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent and explicit OS username are required"))
	}
	if _, err := clients.GetClientByUUID(agent); err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	allowed, err := clients.PrivilegedCapabilityAllowed(agent, "webssh")
	if err != nil || !allowed {
		return nil, connectError(connect.CodePermissionDenied, errors.New("remote control is disabled"))
	}
	if err := verifyConnectTwoFactor(meta, req.Msg.TwoFactor); err != nil {
		return nil, err
	}
	token, snapshot, err := workspace.Default.Create(workspace.Binding{UserID: user, Session: session, AgentID: agent, SystemUser: req.Msg.SystemUser})
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	auditlog.Log(meta.RemoteIP, user, "unlock workspace: "+agent+", OS account: "+req.Msg.SystemUser, "warn")
	response := connect.NewResponse(&workspacev1.UnlockWorkspaceResponse{Authorization: workspaceAuthorization(token, snapshot)})
	// Only the binary data-plane path needs a cookie (images, video and download
	// links cannot set headers). It grants no account, RPC2 or other admin access.
	response.Header().Add("Set-Cookie", (&http.Cookie{Name: workspace.CookieName(agent), Value: token,
		Path: "/api/admin/client/" + url.PathEscape(agent) + "/file", HttpOnly: true, Secure: workspace.Secure(ctx), SameSite: http.SameSiteStrictMode, MaxAge: int(workspace.AbsoluteLifetime.Seconds())}).String())
	response.Header().Set("Cache-Control", "no-store")
	return response, nil
}

func workspaceAuthorization(token string, s workspace.Snapshot) *workspacev1.WorkspaceAuthorization {
	return &workspacev1.WorkspaceAuthorization{GrantToken: token, AgentId: s.Grant.Binding.AgentID, SystemUser: s.Grant.Binding.SystemUser, ExpiresAt: timestamppb.New(s.Grant.ExpiresAt), IdleExpiresAt: timestamppb.New(s.IdleExpiresAt)}
}
func checkWorkspace(ctx context.Context, token string, touch bool) (*workspacev1.WorkspaceAuthorization, error) {
	user, session, err := workspace.Interactive(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	snapshot, err := workspace.Default.Check(token, user, session, "", touch)
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	return workspaceAuthorization(token, snapshot), nil
}
func (s *workspaceService) GetWorkspace(ctx context.Context, req *connect.Request[workspacev1.GetWorkspaceRequest]) (*connect.Response[workspacev1.GetWorkspaceResponse], error) {
	authorization, err := checkWorkspace(ctx, req.Msg.GrantToken, false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.GetWorkspaceResponse{Authorization: authorization}), nil
}
func (s *workspaceService) TouchWorkspace(ctx context.Context, req *connect.Request[workspacev1.TouchWorkspaceRequest]) (*connect.Response[workspacev1.TouchWorkspaceResponse], error) {
	authorization, err := checkWorkspace(ctx, req.Msg.GrantToken, true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.TouchWorkspaceResponse{Authorization: authorization}), nil
}
func (s *workspaceService) LockWorkspace(ctx context.Context, req *connect.Request[workspacev1.LockWorkspaceRequest]) (*connect.Response[workspacev1.LockWorkspaceResponse], error) {
	user, session, err := workspace.Interactive(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	if err := workspace.Default.Lock(req.Msg.GrantToken, user, session); err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	return connect.NewResponse(&workspacev1.LockWorkspaceResponse{Locked: true}), nil
}
