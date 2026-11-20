package connectapi

import (
	"context"
	"encoding/json"
	"net/http"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	v1 "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type passkeyService struct{}

const ceremonyCookie = "__Host-komari_passkey_preauth"

func passkeyOptions(x any) (*structpb.Struct, error) {
	raw, err := json.Marshal(x)
	if err != nil {
		return nil, err
	}
	s := &structpb.Struct{}
	err = s.UnmarshalJSON(raw)
	return s, err
}
func (passkeyService) GetPasskeyStatus(ctx context.Context, _ *connect.Request[v1.GetPasskeyStatusRequest]) (*connect.Response[v1.GetPasskeyStatusResponse], error) {
	return credentialResponse(&v1.GetPasskeyStatusResponse{Enabled: passkeycredential.AvailableAtOrigin(ctx)}), nil
}
func (passkeyService) BeginPasskeyLogin(ctx context.Context, r *connect.Request[v1.BeginPasskeyLoginRequest]) (*connect.Response[v1.BeginPasskeyLoginResponse], error) {
	ip, _ := loginRequestMeta(ctx)
	opt, token, err := passkeycredential.BeginLogin(ctx, r.Msg.Username, ip)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	options, err := passkeyOptions(opt)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	res := credentialResponse(&v1.BeginPasskeyLoginResponse{Options: options})
	res.Header().Add("Set-Cookie", (&http.Cookie{Name: ceremonyCookie, Value: token, Secure: true, HttpOnly: true, Path: "/", MaxAge: int(passkeycredential.CeremonyLifetime.Seconds()), SameSite: http.SameSiteStrictMode}).String())
	return res, nil
}
func (passkeyService) CompletePasskeyLogin(ctx context.Context, r *connect.Request[v1.CompletePasskeyLoginRequest]) (*connect.Response[v1.CompletePasskeyLoginResponse], error) {
	cookie, err := (&http.Request{Header: r.Header()}).Cookie(ceremonyCookie)
	if err != nil || r.Msg.Assertion == nil {
		return nil, credentialConnectError(credential.ErrVerificationFailed)
	}
	raw, err := r.Msg.Assertion.MarshalJSON()
	if err != nil {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	user, err := passkeycredential.CompleteLogin(ctx, cookie.Value, raw)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	ip, ua := loginRequestMeta(ctx)
	session, err := accounts.CreateSession(user, credentialgateway.LoginSessionLifetime, ua, ip, "passkey")
	if err != nil {
		return nil, credentialConnectError(err)
	}
	res := credentialResponse(&v1.CompletePasskeyLoginResponse{Authenticated: true})
	setLoginSession(res, session)
	res.Header().Add("Set-Cookie", (&http.Cookie{Name: ceremonyCookie, Secure: true, HttpOnly: true, Path: "/", MaxAge: -1, SameSite: http.SameSiteStrictMode}).String())
	auditlog.Log(ip, user, "logged in (native passkey)", "login")
	return res, nil
}
func summary(row models.UserPasskey) *v1.PasskeySummary {
	s := &v1.PasskeySummary{CredentialId: row.CredentialID, DisplayName: row.Name, CreatedAt: timestamppb.New(row.CreatedAt)}
	if row.LastUsedAt != nil {
		s.LastUsedAt = timestamppb.New(*row.LastUsedAt)
	}
	return s
}
func (passkeyService) ListPasskeys(ctx context.Context, r *connect.Request[v1.ListPasskeysRequest]) (*connect.Response[v1.ListPasskeysResponse], error) {
	rows, err := credentialgateway.Default.ListPasskeys(ctx, r.Msg.AuthorizationToken)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	res := &v1.ListPasskeysResponse{}
	for _, row := range rows {
		res.Passkeys = append(res.Passkeys, summary(row))
	}
	return credentialResponse(res), nil
}
func (passkeyService) BeginPasskeyRegistration(ctx context.Context, r *connect.Request[v1.BeginPasskeyRegistrationRequest]) (*connect.Response[v1.BeginPasskeyRegistrationResponse], error) {
	opt, id, err := credentialgateway.Default.BeginPasskeyRegistration(ctx, r.Msg.DisplayName, r.Msg.GetAuthorizationToken(), r.Msg.GetPassword())
	if err != nil {
		return nil, credentialConnectError(err)
	}
	options, err := passkeyOptions(opt)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	return credentialResponse(&v1.BeginPasskeyRegistrationResponse{ChallengeId: id, Options: options}), nil
}
func (passkeyService) CompletePasskeyRegistration(ctx context.Context, r *connect.Request[v1.CompletePasskeyRegistrationRequest]) (*connect.Response[v1.CompletePasskeyRegistrationResponse], error) {
	if r.Msg.Attestation == nil {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	raw, err := r.Msg.Attestation.MarshalJSON()
	if err != nil {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	row, err := credentialgateway.Default.CompletePasskeyRegistration(ctx, r.Msg.ChallengeId, raw)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	return credentialResponse(&v1.CompletePasskeyRegistrationResponse{Passkey: summary(row)}), nil
}
func (passkeyService) DeletePasskey(ctx context.Context, r *connect.Request[v1.DeletePasskeyRequest]) (*connect.Response[v1.DeletePasskeyResponse], error) {
	if err := credentialgateway.Default.DeletePasskey(ctx, r.Msg.CredentialId, r.Msg.AuthorizationToken); err != nil {
		return nil, credentialConnectError(err)
	}
	return credentialResponse(&v1.DeletePasskeyResponse{}), nil
}
