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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	enrollmentv1 "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	"github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type enrollmentService struct {
	enrollmentv1connect.UnimplementedEnrollmentServiceHandler
}

// verificationPath is where an operator approves a machine. It is rendered by
// the panel UI, which is also what forces a panel login before approving.
const verificationPath = "/bind"

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
	refreshToken := req.Msg.GetRefreshToken()
	protocol := 1
	requestID := "legacy:" + refreshToken
	var expected *uint64
	var record models.AgentCredential
	var err error
	version, versionErr := refreshProtocolVersion(req.Msg.GetProof())
	if versionErr != nil {
		return nil, connectError(connect.CodeUnauthenticated, errors.New("unsupported refresh proof protocol"))
	}
	if version == 2 {
		protocol = 2
		record, err = enrollment.LookupRefresh(ctx, refreshToken)
		if err == nil {
			requestID, expected, err = verifyRefreshV2(req.Msg.Proof, record, refreshToken)
		}
	} else {
		record, err = enrollment.ValidateRefreshToken(refreshToken)
		if err == nil {
			err = verifyRefreshProof(req.Msg.GetProof(), record.AgentPublicKey, record.Client, refreshToken)
		}
	}
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, errors.New("invalid refresh proof or credential"))
	}
	issued, err := enrollment.RotateVerified(ctx, record, refreshToken, requestID, expected, protocol)
	if err != nil {
		if errors.Is(err, enrollment.ErrRefreshConflict) {
			return nil, connectError(connect.CodeAborted, err)
		}
		if errors.Is(err, enrollment.ErrRefreshReuse) {
			auditlog.Log("", "", "agent credential chain revoked after authenticated refresh reuse: client="+record.Client, "warn")
		}
		if errors.Is(err, enrollment.ErrCredentialRejected) || errors.Is(err, enrollment.ErrRefreshReuse) {
			return nil, connectError(connect.CodeUnauthenticated, err)
		}
		return nil, connectError(connect.CodeInternal, errors.New("credential rotation failed"))
	}

	creds := &enrollmentv1.AgentCredentials{
		AgentId:               issued.AgentID,
		AccessToken:           issued.AccessToken,
		AccessTokenExpiresAt:  timestamppb.New(issued.AccessTokenExpiresAt),
		RefreshToken:          issued.RefreshToken,
		RefreshTokenExpiresAt: timestamppb.New(issued.RefreshTokenExpiresAt),
		Scopes:                issued.Scopes,
	}

	// V2 signs the complete deterministic credential message and binds it to
	// the request ID and generation. Legacy Agents retain their existing payload.
	response := &enrollmentv1.RefreshCredentialsResponse{Credentials: creds}
	payload := []byte(issued.AgentID + "\n" + issued.AccessToken)
	if protocol == 2 {
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(creds)
		if err != nil {
			return nil, connectError(connect.CodeInternal, errors.New("credential response encoding failed"))
		}
		digest := sha256.Sum256(body)
		payload, err = json.Marshal(refreshV2Response{Protocol: 2, AgentID: issued.AgentID, RequestID: issued.RequestID, Generation: issued.Generation, CredentialsSHA256: hex.EncodeToString(digest[:])})
		if err != nil {
			return nil, connectError(connect.CodeInternal, err)
		}
	}
	keyID, algorithm, sig, signErr := enrollment.Sign(payload)
	if signErr != nil {
		return nil, connectError(connect.CodeInternal, errors.New("credential response signing unavailable; retry same request"))
	}
	response.Proof = &securityv1.SignedEnvelope{Payload: payload, Signatures: []*securityv1.Signature{{Algorithm: securityv1.SignatureAlgorithm(algorithm), Value: sig, KeyId: keyID}}}

	return connect.NewResponse(response), nil
}

func (s *enrollmentService) RevokeCredentials(ctx context.Context, req *connect.Request[enrollmentv1.RevokeCredentialsRequest]) (*connect.Response[enrollmentv1.RevokeCredentialsResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectCredential(ctx, req.Msg.GetTwoFactor(), "enrollment:revoke", req.Msg.GetAgentId()); err != nil {
		return nil, err
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
	if _, err := enrollment.ActiveSigningKey(); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	keys, err := enrollment.ListControlPlaneKeys()
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

// verifyRefreshProof checks the signature and bound payload an Agent makes over its refresh.
func verifyRefreshProof(envelope *securityv1.SignedEnvelope, storedKey, clientUUID, expectedRefreshToken string) error {
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

	validSig := false
	for _, signature := range envelope.GetSignatures() {
		if signature.GetAlgorithm() != securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519 {
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(raw), payload, signature.GetValue()) {
			sigHash := hex.EncodeToString(signature.GetValue())
			if !recordAndVerifyNonce("refresh-sig:"+sigHash, 120*time.Second) {
				return errors.New("the refresh proof signature was already used (replay rejected)")
			}
			validSig = true
			break
		}
	}
	if !validSig {
		return errors.New("the refresh proof signature is not valid")
	}

	// 1. komari-agent standard wire format:
	// lines[0]: "komari-refresh-v1"
	// lines[1]: AgentID
	// lines[2]: RefreshToken
	// lines[3]: RFC3339 Timestamp
	lines := strings.Split(string(payload), "\n")
	if len(lines) >= 4 && lines[0] == "komari-refresh-v1" {
		agentID := lines[1]
		token := lines[2]
		tsStr := lines[3]

		if agentID != clientUUID {
			return errors.New("the refresh proof agent ID does not match this agent")
		}
		if token != expectedRefreshToken {
			return errors.New("the refresh proof token does not match the presented refresh token")
		}
		t, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			return errors.New("invalid timestamp in refresh proof")
		}
		if math.Abs(time.Since(t).Seconds()) > 120 {
			return errors.New("the refresh proof timestamp drift exceeds allowed window (120s)")
		}
		return nil
	}

	// 2. Structured JSON payload (e.g. RefreshProofPayload / SignedInstruction)
	var proofObj struct {
		AgentID      string `json:"agent_id"`
		RefreshToken string `json:"refresh_token"`
		Timestamp    string `json:"timestamp"`
		Nonce        string `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &proofObj); err == nil && proofObj.AgentID != "" {
		if proofObj.AgentID != clientUUID {
			return errors.New("the refresh proof agent ID does not match this agent")
		}
		if proofObj.RefreshToken != "" && proofObj.RefreshToken != expectedRefreshToken {
			return errors.New("the refresh proof token does not match the presented refresh token")
		}
		if proofObj.Timestamp != "" {
			t, err := time.Parse(time.RFC3339, proofObj.Timestamp)
			if err == nil && math.Abs(time.Since(t).Seconds()) > 120 {
				return errors.New("the refresh proof timestamp drift exceeds allowed window (120s)")
			}
		}
		if proofObj.Nonce != "" && !recordAndVerifyNonce("proof-nonce:"+proofObj.Nonce, 120*time.Second) {
			return errors.New("the refresh proof nonce was already used (replay rejected)")
		}
		return nil
	}

	// 3. Fallback: must at least contain both clientUUID and expectedRefreshToken
	if !strings.Contains(string(payload), clientUUID) {
		return errors.New("the refresh proof does not match this agent")
	}
	if !strings.Contains(string(payload), expectedRefreshToken) {
		return errors.New("the refresh proof does not match the presented refresh token")
	}

	return nil
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
