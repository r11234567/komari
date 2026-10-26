package connectapi

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	credentialv1 "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const passwordCookieName = "komari_password_preauth"
const passwordServicePath = "/komari.credential.v1.PasswordLoginService/"

type passwordLoginService struct{}

func passwordCookie[T any](response *connect.Response[T], value string, age int) {
	cookie := &http.Cookie{Name: passwordCookieName, Value: value, Path: passwordServicePath, MaxAge: age, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
	response.Header().Add("Set-Cookie", cookie.String())
}

func passwordToken(header http.Header) string {
	request := &http.Request{Header: header}
	cookie, err := request.Cookie(passwordCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func loginRequestMeta(ctx context.Context) (string, string) {
	if meta := rpc.MetaFromContext(ctx); meta != nil {
		return meta.RemoteIP, meta.UserAgent
	}
	return "", ""
}

func loginSessionCookie[T any](ctx context.Context, response *connect.Response[T], session, user string) {
	response.Header().Add("Set-Cookie", (&http.Cookie{Name: "session_token", Value: session, Path: "/", MaxAge: credentialgateway.LoginSessionLifetime, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}).String())
	passwordCookie(response, "", -1)
	ip, _ := loginRequestMeta(ctx)
	auditlog.Log(ip, user, "logged in (trusted password flow)", "login")
}

func (s *passwordLoginService) BeginPasswordLogin(ctx context.Context, req *connect.Request[credentialv1.BeginPasswordLoginRequest]) (*connect.Response[credentialv1.BeginPasswordLoginResponse], error) {
	ip, agent := loginRequestMeta(ctx)
	// Replacing a flow does not leave another usable cookie-bound ceremony.
	if old := passwordToken(req.Header()); old != "" {
		credentialgateway.Default.CancelPassword(old)
	}
	started, err := credentialgateway.Default.BeginPassword(ctx, req.Msg.Username, req.Msg.Password, ip, agent)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	result := &credentialv1.BeginPasswordLoginResponse{Authenticated: started.Session != ""}
	response := credentialResponse(result)
	if result.Authenticated {
		loginSessionCookie(ctx, response, started.Session, started.UserID)
		return response, nil
	}
	result.ExpiresAt = timestamppb.New(started.ExpiresAt)
	for _, item := range started.Providers {
		result.Providers = append(result.Providers, loginProvider(item))
	}
	passwordCookie(response, started.Preauthentication, int(credentialgateway.PasswordLoginLifetime.Seconds()))
	return response, nil
}

func loginProvider(item credential.Descriptor) *credentialv1.TrustedCredentialProvider {
	kinds := map[string]credentialv1.CredentialProviderKind{"legacy_totp": credentialv1.CredentialProviderKind_CREDENTIAL_PROVIDER_KIND_LEGACY_TOTP, "email_otp": credentialv1.CredentialProviderKind_CREDENTIAL_PROVIDER_KIND_EMAIL_OTP, "telegram_otp": credentialv1.CredentialProviderKind_CREDENTIAL_PROVIDER_KIND_TELEGRAM_OTP, "plugin": credentialv1.CredentialProviderKind_CREDENTIAL_PROVIDER_KIND_PLUGIN}
	return &credentialv1.TrustedCredentialProvider{Id: item.ID, DisplayName: item.DisplayName, Kind: kinds[item.Kind], Purposes: []credentialv1.CredentialPurpose{credentialv1.CredentialPurpose_CREDENTIAL_PURPOSE_PASSWORD_LOGIN}}
}

func (s *passwordLoginService) BeginLoginVerification(ctx context.Context, req *connect.Request[credentialv1.BeginLoginVerificationRequest]) (*connect.Response[credentialv1.BeginLoginVerificationResponse], error) {
	c, err := credentialgateway.Default.BeginPasswordVerification(ctx, passwordToken(req.Header()), req.Msg.ProviderId)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	prompt := &structpb.Struct{}
	if prompt.UnmarshalJSON(c.Prompt.Data) != nil {
		return nil, credentialConnectError(credential.ErrProviderUnavailable)
	}
	return credentialResponse(&credentialv1.BeginLoginVerificationResponse{ChallengeId: c.ID, ExpiresAt: timestamppb.New(c.ExpiresAt), PromptType: c.Prompt.Type, Prompt: prompt}), nil
}

func (s *passwordLoginService) CompletePasswordLogin(ctx context.Context, req *connect.Request[credentialv1.CompletePasswordLoginRequest]) (*connect.Response[credentialv1.CompletePasswordLoginResponse], error) {
	if req.Msg.Proof == nil {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	proof, err := req.Msg.Proof.MarshalJSON()
	if err != nil {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	ip, agent := loginRequestMeta(ctx)
	session, user, err := credentialgateway.Default.CompletePassword(ctx, passwordToken(req.Header()), req.Msg.ChallengeId, proof, ip, agent)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	response := credentialResponse(&credentialv1.CompletePasswordLoginResponse{Authenticated: true})
	loginSessionCookie(ctx, response, session, user)
	return response, nil
}

func (s *passwordLoginService) CancelPasswordLogin(_ context.Context, req *connect.Request[credentialv1.CancelPasswordLoginRequest]) (*connect.Response[credentialv1.CancelPasswordLoginResponse], error) {
	response := credentialResponse(&credentialv1.CancelPasswordLoginResponse{Cancelled: credentialgateway.Default.CancelPassword(passwordToken(req.Header()))})
	passwordCookie(response, "", -1)
	return response, nil
}
