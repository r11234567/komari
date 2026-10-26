package connectapi

import (
	"context"
	"encoding/json"
	"errors"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	"github.com/komari-monitor/komari/internal/notificationotp"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	credentialv1 "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type credentialService struct{}

var credentialPurposes = map[credentialv1.CredentialPurpose]credential.Purpose{
	credentialv1.CredentialPurpose_CREDENTIAL_PURPOSE_UNSPECIFIED:         "",
	credentialv1.CredentialPurpose_CREDENTIAL_PURPOSE_BIND_CONTACT:        credential.BindContact,
	credentialv1.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGE_CREDENTIALS:  credential.ManageCredentials,
	credentialv1.CredentialPurpose_CREDENTIAL_PURPOSE_UNLOCK_WORKSPACE:    credential.UnlockWorkspace,
	credentialv1.CredentialPurpose_CREDENTIAL_PURPOSE_LOGIN_SETTINGS:      credential.LoginSettings,
	credentialv1.CredentialPurpose_CREDENTIAL_PURPOSE_SENSITIVE_OPERATION: credential.SensitiveOperation,
}

func protoCredentialPurpose(p credential.Purpose) credentialv1.CredentialPurpose {
	for key, value := range credentialPurposes {
		if value == p {
			return key
		}
	}
	return credentialv1.CredentialPurpose_CREDENTIAL_PURPOSE_UNSPECIFIED
}

func (s *credentialService) ListTrustedCredentialProviders(ctx context.Context, req *connect.Request[credentialv1.ListTrustedCredentialProvidersRequest]) (*connect.Response[credentialv1.ListTrustedCredentialProvidersResponse], error) {
	purpose, ok := credentialPurposes[req.Msg.Purpose]
	if !ok {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	items, err := credentialgateway.Default.List(ctx, purpose)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	result := &credentialv1.ListTrustedCredentialProvidersResponse{}
	kinds := map[string]credentialv1.CredentialProviderKind{
		"legacy_totp":  credentialv1.CredentialProviderKind_CREDENTIAL_PROVIDER_KIND_LEGACY_TOTP,
		"email_otp":    credentialv1.CredentialProviderKind_CREDENTIAL_PROVIDER_KIND_EMAIL_OTP,
		"telegram_otp": credentialv1.CredentialProviderKind_CREDENTIAL_PROVIDER_KIND_TELEGRAM_OTP,
		"plugin":       credentialv1.CredentialProviderKind_CREDENTIAL_PROVIDER_KIND_PLUGIN,
	}
	for _, item := range items {
		provider := &credentialv1.TrustedCredentialProvider{Id: item.ID, DisplayName: item.DisplayName, Kind: kinds[item.Kind]}
		for _, p := range item.Purposes {
			provider.Purposes = append(provider.Purposes, protoCredentialPurpose(p))
		}
		result.Providers = append(result.Providers, provider)
	}
	return credentialResponse(result), nil
}

func (s *credentialService) BeginVerification(ctx context.Context, req *connect.Request[credentialv1.BeginVerificationRequest]) (*connect.Response[credentialv1.BeginVerificationResponse], error) {
	purpose, ok := credentialPurposes[req.Msg.Purpose]
	if !ok || purpose == "" {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	parameters := json.RawMessage(`{}`)
	if req.Msg.Parameters != nil {
		var err error
		parameters, err = req.Msg.Parameters.MarshalJSON()
		if err != nil {
			return nil, credentialConnectError(credential.ErrInvalidArgument)
		}
	}
	challenge, err := credentialgateway.Default.Begin(ctx, credential.Request{ProviderID: req.Msg.ProviderId, Purpose: purpose, Resource: req.Msg.Resource, Parameters: parameters})
	if err != nil {
		return nil, credentialConnectError(err)
	}
	prompt := &structpb.Struct{}
	if err := prompt.UnmarshalJSON(challenge.Prompt.Data); err != nil {
		return nil, credentialConnectError(credential.ErrProviderUnavailable)
	}
	return credentialResponse(&credentialv1.BeginVerificationResponse{ChallengeId: challenge.ID, ExpiresAt: timestamppb.New(challenge.ExpiresAt), PromptType: challenge.Prompt.Type, Prompt: prompt}), nil
}

func (s *credentialService) CompleteVerification(ctx context.Context, req *connect.Request[credentialv1.CompleteVerificationRequest]) (*connect.Response[credentialv1.CompleteVerificationResponse], error) {
	if req.Msg.Proof == nil {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	proof, err := req.Msg.Proof.MarshalJSON()
	if err != nil {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	a, err := credentialgateway.Default.Complete(ctx, req.Msg.ChallengeId, proof)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	return credentialResponse(&credentialv1.CompleteVerificationResponse{Authorization: &credentialv1.CredentialAuthorization{GrantToken: a.Token, ProviderId: a.ProviderID, Purpose: protoCredentialPurpose(a.Purpose), Resource: a.Resource, ExpiresAt: timestamppb.New(a.ExpiresAt)}}), nil
}

func (s *credentialService) CancelVerification(ctx context.Context, req *connect.Request[credentialv1.CancelVerificationRequest]) (*connect.Response[credentialv1.CancelVerificationResponse], error) {
	cancelled, err := credentialgateway.Default.Cancel(ctx, req.Msg.ChallengeId)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	return credentialResponse(&credentialv1.CancelVerificationResponse{Cancelled: cancelled}), nil
}

func credentialResponse[T any](msg *T) *connect.Response[T] {
	r := connect.NewResponse(msg)
	r.Header().Set("Cache-Control", "no-store")
	return r
}

func credentialConnectError(err error) error {
	code := connect.CodeInternal
	switch {
	case errors.Is(err, accounts.ErrLastCredential):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	case errors.Is(err, credential.ErrInvalidArgument):
		code = connect.CodeInvalidArgument
	case errors.Is(err, credential.ErrUnauthenticated), errors.Is(err, credential.ErrVerificationFailed):
		code = connect.CodeUnauthenticated
	case errors.Is(err, credential.ErrProviderUnavailable):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, credential.ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, credential.ErrBusy):
		code = connect.CodeAborted
	case errors.Is(err, credential.ErrLimit):
		code = connect.CodeResourceExhausted
	default:
		err = errors.New("credential operation failed")
	}
	return connect.NewError(code, err)
}

func (s *credentialService) GetCredentialContacts(ctx context.Context, _ *connect.Request[credentialv1.GetCredentialContactsRequest]) (*connect.Response[credentialv1.GetCredentialContactsResponse], error) {
	settings, err := credentialgateway.Default.Contacts(ctx)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	result := &credentialv1.GetCredentialContactsResponse{PasswordBootstrapAllowed: settings.PasswordBootstrapAllowed}
	for _, contact := range settings.Contacts {
		result.Contacts = append(result.Contacts, &credentialv1.CredentialContact{Kind: contact.Kind, MaskedAddress: notificationotp.MaskAddress(contact.Kind, contact.Address), Transport: contact.Transport, VerifiedAt: timestamppb.New(contact.VerifiedAt)})
	}
	for _, transport := range settings.Transports {
		result.Transports = append(result.Transports, &credentialv1.CredentialTransport{Id: transport.ID, Kinds: transport.Kinds})
	}
	return credentialResponse(result), nil
}

func (s *credentialService) BeginContactBinding(ctx context.Context, req *connect.Request[credentialv1.BeginContactBindingRequest]) (*connect.Response[credentialv1.BeginContactBindingResponse], error) {
	if len(req.Msg.Password) > 1024 {
		return nil, credentialConnectError(credential.ErrInvalidArgument)
	}
	c, err := credentialgateway.Default.BeginContactBinding(ctx, notificationotp.Destination{Kind: req.Msg.Kind, Address: req.Msg.Address, Transport: req.Msg.Transport}, req.Msg.AuthorizationToken, req.Msg.Password)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	prompt := &structpb.Struct{}
	if err := prompt.UnmarshalJSON(c.Prompt.Data); err != nil {
		return nil, credentialConnectError(credential.ErrProviderUnavailable)
	}
	return credentialResponse(&credentialv1.BeginContactBindingResponse{ChallengeId: c.ID, ExpiresAt: timestamppb.New(c.ExpiresAt), PromptType: c.Prompt.Type, Prompt: prompt}), nil
}

func (s *credentialService) SaveCredentialContact(ctx context.Context, req *connect.Request[credentialv1.SaveCredentialContactRequest]) (*connect.Response[credentialv1.SaveCredentialContactResponse], error) {
	c, err := credentialgateway.Default.SaveContact(ctx, notificationotp.Destination{Kind: req.Msg.Kind, Address: req.Msg.Address, Transport: req.Msg.Transport}, req.Msg.GrantToken)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	return credentialResponse(&credentialv1.SaveCredentialContactResponse{Contact: &credentialv1.CredentialContact{Kind: c.Kind, MaskedAddress: notificationotp.MaskAddress(c.Kind, c.Address), Transport: c.Transport, VerifiedAt: timestamppb.New(c.VerifiedAt)}}), nil
}

func (s *credentialService) DeleteCredentialContact(ctx context.Context, req *connect.Request[credentialv1.DeleteCredentialContactRequest]) (*connect.Response[credentialv1.DeleteCredentialContactResponse], error) {
	deleted, err := credentialgateway.Default.DeleteContact(ctx, req.Msg.Kind, req.Msg.GrantToken)
	if err != nil {
		return nil, credentialConnectError(err)
	}
	return credentialResponse(&credentialv1.DeleteCredentialContactResponse{Deleted: deleted}), nil
}
