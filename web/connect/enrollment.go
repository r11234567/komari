package connectapi

// Enrollment service.
//
// Two procedures here are reachable without credentials, which is unavoidable:
// an Agent that has never enrolled has nothing to authenticate with. They are
// safe to expose because neither one grants anything on its own. Beginning an
// attempt only records a request that a human must still approve, and polling
// requires a device code the server generated and handed to exactly one
// caller. The decision that matters happens in the panel, behind a session.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	enrollmentv1 "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	"github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type enrollmentService struct {
	enrollmentv1connect.UnimplementedEnrollmentServiceHandler
}

// verificationPath is where an operator approves a machine. It is rendered by
// the panel UI, which is also what forces a panel login before approving.
const verificationPath = "/admin/agents/enroll"

func (s *enrollmentService) BeginEnrollment(ctx context.Context, req *connect.Request[enrollmentv1.BeginEnrollmentRequest]) (*connect.Response[enrollmentv1.BeginEnrollmentResponse], error) {
	key := req.Msg.GetAgentPublicKey()
	if key == nil || len(key.GetValue()) == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("an agent public key is required"))
	}
	// Only algorithms the server can actually verify a proof with are
	// accepted. Recording a key that cannot be checked would produce
	// credentials whose binding to the host is decorative.
	if key.GetAlgorithm() != securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519 {
		return nil, connectError(connect.CodeInvalidArgument,
			fmt.Errorf("unsupported agent key algorithm %s", key.GetAlgorithm()))
	}
	if len(key.GetValue()) != ed25519.PublicKeySize {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent public key has an unexpected size"))
	}

	device := req.Msg.GetDevice()
	meta := rpc.MetaFromContext(ctx)
	remoteIP := ""
	if meta != nil {
		remoteIP = meta.RemoteIP
	}
	attempt, err := enrollment.Begin(key.GetValue(), key.GetKeyId(), int32(key.GetAlgorithm()), enrollment.Attempt{
		Hostname:        device.GetHostname(),
		OperatingSystem: device.GetOperatingSystem(),
		Architecture:    device.GetArchitecture(),
		AgentVersion:    device.GetAgentVersion(),
		HostFingerprint: device.GetFingerprint(),
		RequestedScopes: req.Msg.GetRequestedScopes(),
		RemoteIP:        remoteIP,
	})
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}

	// The attempt is logged before anyone approves it, so an unexpected
	// enrollment request is visible even if it is never granted.
	auditlog.Log(remoteIP, "", fmt.Sprintf("agent enrollment requested: host=%s code=%s",
		attempt.Hostname, attempt.UserCode), "info")

	base := panelBaseURL(req)
	return connect.NewResponse(&enrollmentv1.BeginEnrollmentResponse{
		DeviceCode:              attempt.DeviceCode,
		UserCode:                attempt.UserCode,
		VerificationUri:         base + verificationPath,
		VerificationUriComplete: base + verificationPath + "?code=" + attempt.UserCode,
		ExpiresAt:               timestamppb.New(attempt.ExpiresAt),
		PollInterval:            durationpb.New(enrollment.PollInterval),
	}), nil
}

func (s *enrollmentService) PollEnrollment(ctx context.Context, req *connect.Request[enrollmentv1.PollEnrollmentRequest]) (*connect.Response[enrollmentv1.PollEnrollmentResponse], error) {
	attempt, issued, err := enrollment.Poll(req.Msg.GetDeviceCode())
	if errors.Is(err, enrollment.ErrSlowDown) {
		return connect.NewResponse(&enrollmentv1.PollEnrollmentResponse{
			State:        enrollmentv1.EnrollmentState_ENROLLMENT_STATE_SLOW_DOWN,
			PollInterval: durationpb.New(enrollment.PollInterval * 2),
		}), nil
	}
	if err != nil {
		// A device code that does not resolve is reported as expired rather
		// than not-found, so polling cannot be used to learn which codes exist.
		return connect.NewResponse(&enrollmentv1.PollEnrollmentResponse{
			State:        enrollmentv1.EnrollmentState_ENROLLMENT_STATE_EXPIRED,
			PollInterval: durationpb.New(enrollment.PollInterval),
		}), nil
	}

	response := &enrollmentv1.PollEnrollmentResponse{
		State:        enrollmentStateToProto(attempt.State),
		PollInterval: durationpb.New(enrollment.PollInterval),
	}
	if issued != nil {
		response.Credentials = &enrollmentv1.AgentCredentials{
			AgentId:               issued.AgentID,
			AccessToken:           issued.AccessToken,
			AccessTokenExpiresAt:  timestamppb.New(issued.AccessTokenExpiresAt),
			RefreshToken:          issued.RefreshToken,
			RefreshTokenExpiresAt: timestamppb.New(issued.RefreshTokenExpiresAt),
			Scopes:                issued.Scopes,
		}
		if issued.PreviousTokenExpires != nil {
			response.Credentials.PreviousTokenExpiresAt = timestamppb.New(*issued.PreviousTokenExpires)
		}
		auditlog.Log(attempt.RemoteIP, attempt.ApprovedBy,
			fmt.Sprintf("agent enrollment completed: client=%s host=%s", issued.AgentID, attempt.Hostname), "warn")
	}
	if attempt.State == enrollment.StateDenied {
		response.Error = &commonv1.ErrorDetail{Code: "ENROLLMENT_DENIED", Message: "the request was denied in the panel"}
	}
	return connect.NewResponse(response), nil
}

func (s *enrollmentService) RefreshCredentials(ctx context.Context, req *connect.Request[enrollmentv1.RefreshCredentialsRequest]) (*connect.Response[enrollmentv1.RefreshCredentialsResponse], error) {
	issued, record, err := enrollment.Refresh(req.Msg.GetRefreshToken())
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	// The proof is what makes a leaked refresh token insufficient on its own.
	// It is verified after the token resolves, because the key to check
	// against is the one recorded for that machine.
	if err := verifyRefreshProof(req.Msg.GetProof(), record.AgentPublicKey, record.Client); err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	return connect.NewResponse(&enrollmentv1.RefreshCredentialsResponse{
		Credentials: &enrollmentv1.AgentCredentials{
			AgentId:               issued.AgentID,
			AccessToken:           issued.AccessToken,
			AccessTokenExpiresAt:  timestamppb.New(issued.AccessTokenExpiresAt),
			RefreshToken:          issued.RefreshToken,
			RefreshTokenExpiresAt: timestamppb.New(issued.RefreshTokenExpiresAt),
			Scopes:                issued.Scopes,
		},
	}), nil
}

func (s *enrollmentService) RevokeCredentials(ctx context.Context, req *connect.Request[enrollmentv1.RevokeCredentialsRequest]) (*connect.Response[enrollmentv1.RevokeCredentialsResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	if strings.TrimSpace(req.Msg.GetAgentId()) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("an agent ID is required"))
	}
	if err := enrollment.Revoke(req.Msg.GetAgentId(), req.Msg.GetReason()); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	auditlog.Log(meta.RemoteIP, meta.Principal.UserUUID,
		fmt.Sprintf("agent credentials revoked: client=%s reason=%s", req.Msg.GetAgentId(), req.Msg.GetReason()), "warn")
	return connect.NewResponse(&enrollmentv1.RevokeCredentialsResponse{Accepted: true}), nil
}

func (s *enrollmentService) GetTrustBundle(ctx context.Context, req *connect.Request[enrollmentv1.GetTrustBundleRequest]) (*connect.Response[enrollmentv1.GetTrustBundleResponse], error) {
	keys, err := listControlPlaneKeys()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	published := make([]*securityv1.PublicKey, 0, len(keys))
	for _, key := range keys {
		raw, decodeErr := base64.StdEncoding.DecodeString(key.PublicKey)
		if decodeErr != nil {
			continue
		}
		entry := &securityv1.PublicKey{
			Algorithm: securityv1.SignatureAlgorithm(key.Algorithm),
			Value:     raw,
			KeyId:     key.KeyID,
			NotBefore: timestamppb.New(key.NotBefore),
		}
		if key.NotAfter != nil {
			entry.NotAfter = timestamppb.New(*key.NotAfter)
		}
		published = append(published, entry)
	}
	return connect.NewResponse(&enrollmentv1.GetTrustBundleResponse{
		SigningKeys: published,
		Policy: &securityv1.VerificationPolicy{
			AcceptedAlgorithms: []securityv1.SignatureAlgorithm{
				securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519,
			},
			// One verifying signature is sufficient today. Requiring all would
			// break the moment a second algorithm is introduced, which is
			// exactly when dual-signing needs to work.
			RequireAll:        false,
			MinimumSignatures: 1,
		},
		RefreshAfter: durationpb.New(24 * time.Hour),
	}), nil
}

// verifyRefreshProof checks the signature an Agent makes over its refresh.
func verifyRefreshProof(envelope *securityv1.SignedEnvelope, storedKey, clientUUID string) error {
	if envelope == nil || len(envelope.GetSignatures()) == 0 {
		return errors.New("a signed refresh proof is required")
	}
	raw, err := base64.StdEncoding.DecodeString(storedKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return errors.New("the stored agent key cannot verify this proof")
	}
	payload := envelope.GetPayload()
	if len(payload) == 0 {
		return errors.New("the refresh proof has no payload")
	}
	// The payload must name this machine, so a proof captured from one Agent
	// cannot be replayed to refresh another's credentials.
	if !strings.Contains(string(payload), clientUUID) {
		return errors.New("the refresh proof does not match this agent")
	}
	for _, signature := range envelope.GetSignatures() {
		if signature.GetAlgorithm() != securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519 {
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(raw), payload, signature.GetValue()) {
			return nil
		}
	}
	return errors.New("the refresh proof signature is not valid")
}

func enrollmentStateToProto(state int32) enrollmentv1.EnrollmentState {
	switch state {
	case enrollment.StatePending:
		return enrollmentv1.EnrollmentState_ENROLLMENT_STATE_PENDING
	case enrollment.StateApproved:
		return enrollmentv1.EnrollmentState_ENROLLMENT_STATE_APPROVED
	case enrollment.StateDenied:
		return enrollmentv1.EnrollmentState_ENROLLMENT_STATE_DENIED
	case enrollment.StateExpired:
		return enrollmentv1.EnrollmentState_ENROLLMENT_STATE_EXPIRED
	default:
		return enrollmentv1.EnrollmentState_ENROLLMENT_STATE_UNSPECIFIED
	}
}

// panelBaseURL reconstructs the address the Agent used.
//
// Echoing back what the caller reached is what keeps the printed link usable
// behind a reverse proxy or on a non-default port, where a configured value
// would be wrong for exactly the deployments that need it most.
func panelBaseURL(req connect.AnyRequest) string {
	header := req.Header()
	scheme := "https"
	if forwarded := header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = strings.Split(forwarded, ",")[0]
	}
	host := header.Get("X-Forwarded-Host")
	if host == "" {
		host = header.Get("Host")
	}
	if host == "" {
		if parsed := req.Peer().Addr; parsed != "" {
			host = parsed
		}
	}
	return strings.TrimSuffix(scheme+"://"+strings.TrimSpace(host), "/")
}
