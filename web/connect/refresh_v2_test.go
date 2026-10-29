package connectapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/models"
	enrollmentv1 "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	enrollmentconnect "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func refreshFixture(t *testing.T) (models.AgentCredential, string, ed25519.PrivateKey, enrollmentconnect.EnrollmentServiceClient) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	token := "synthetic-refresh-" + uuid.NewString()
	digest := sha256.Sum256([]byte(token))
	access := sha256.Sum256([]byte("synthetic-access" + token))
	record := models.AgentCredential{Client: uuid.NewString(), RefreshTokenHash: hex.EncodeToString(digest[:]), AccessTokenHash: hex.EncodeToString(access[:]),
		AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(180 * 24 * time.Hour), AgentPublicKey: base64.StdEncoding.EncodeToString(public), Scopes: "[]"}
	require.NoError(t, dbcore.GetDBInstance().Create(&record).Error)
	_, handler := enrollmentconnect.NewEnrollmentServiceHandler(&enrollmentService{})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return record, token, private, enrollmentconnect.NewEnrollmentServiceClient(server.Client(), server.URL)
}

func refreshRequest(t *testing.T, record models.AgentCredential, token, id string, generation *uint64, key ed25519.PrivateKey) *connect.Request[enrollmentv1.RefreshCredentialsRequest] {
	t.Helper()
	payload, err := json.Marshal(refreshV2Request{Protocol: 2, AgentID: record.Client, RefreshToken: token, RequestID: id, Generation: generation, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Nonce: uuid.NewString()})
	require.NoError(t, err)
	return connect.NewRequest(&enrollmentv1.RefreshCredentialsRequest{RefreshToken: token, Proof: &securityv1.SignedEnvelope{Payload: payload, Signatures: []*securityv1.Signature{{Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: ed25519.Sign(key, payload)}}}})
}

func TestCredentialRefreshV2SignedFullResponseAndLostResponseRecovery(t *testing.T) {
	record, token, key, client := refreshFixture(t)
	id := uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := client.RefreshCredentials(ctx, refreshRequest(t, record, token, id, nil, key))
	require.NoError(t, err)
	require.NotNil(t, first.Msg.Proof)
	var payload refreshV2Response
	require.NoError(t, json.Unmarshal(first.Msg.Proof.Payload, &payload))
	require.Equal(t, record.Client, payload.AgentID)
	require.Equal(t, id, payload.RequestID)
	require.Equal(t, uint64(1), payload.Generation)
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(first.Msg.Credentials)
	require.NoError(t, err)
	digest := sha256.Sum256(body)
	require.Equal(t, hex.EncodeToString(digest[:]), payload.CredentialsSHA256)
	signing, err := enrollment.ActiveSigningKey()
	require.NoError(t, err)
	require.True(t, ed25519.Verify(signing.PublicKey, first.Msg.Proof.Payload, first.Msg.Proof.Signatures[0].Value))
	zero := uint64(0)
	recovered, err := client.RefreshCredentials(ctx, refreshRequest(t, record, token, id, &zero, key))
	require.NoError(t, err)
	require.True(t, proto.Equal(first.Msg.Credentials, recovered.Msg.Credentials), "a lost response must not mint a second pair")
	oldAccess := "synthetic-access" + token
	resolved, err := enrollment.ResolveAccessToken(oldAccess)
	require.NoError(t, err)
	require.Equal(t, record.Client, resolved)
	require.NoError(t, enrollment.Revoke(record.Client, "synthetic CI revocation"))
	_, err = enrollment.ResolveAccessToken(oldAccess)
	require.Error(t, err)
	_, err = enrollment.ResolveAccessToken(first.Msg.Credentials.AccessToken)
	require.Error(t, err)
	_, err = client.RefreshCredentials(ctx, refreshRequest(t, record, token, id, &zero, key))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}

func TestCredentialRefreshV2InvalidProofCannotRevokeOrRotate(t *testing.T) {
	record, token, key, client := refreshFixture(t)
	request := refreshRequest(t, record, token, uuid.NewString(), nil, key)
	request.Msg.Proof.Signatures[0].Value[0] ^= 1
	_, err := client.RefreshCredentials(context.Background(), request)
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	var current models.AgentCredential
	require.NoError(t, dbcore.GetDBInstance().First(&current, "client = ?", record.Client).Error)
	require.Equal(t, record.RefreshTokenHash, current.RefreshTokenHash)
	require.Zero(t, current.Generation)
	require.Nil(t, current.RevokedAt)
	good := refreshRequest(t, record, token, uuid.NewString(), nil, key)
	_, err = client.RefreshCredentials(context.Background(), good)
	require.NoError(t, err)
	// Replaying an identical captured signed request is rejected before the
	// reuse detector can mutate the machine's state.
	_, err = client.RefreshCredentials(context.Background(), good)
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	require.NoError(t, dbcore.GetDBInstance().First(&current, "client = ?", record.Client).Error)
	require.Nil(t, current.RevokedAt)
}

func TestCredentialRefreshV2RejectsExpiredHandoffAndProtocolDowngrade(t *testing.T) {
	record, token, key, client := refreshFixture(t)
	// The one-minute handoff must never resurrect an already expired AT.
	require.NoError(t, dbcore.GetDBInstance().Model(&record).Update("access_expires_at", time.Now().Add(-time.Second)).Error)
	first, err := client.RefreshCredentials(context.Background(), refreshRequest(t, record, token, uuid.NewString(), nil, key))
	require.NoError(t, err)
	_, err = enrollment.ResolveAccessToken("synthetic-access" + token)
	require.Error(t, err)
	legacyPayload := []byte("komari-refresh-v1\n" + record.Client + "\n" + first.Msg.Credentials.RefreshToken + "\n" + time.Now().UTC().Format(time.RFC3339))
	legacy := connect.NewRequest(&enrollmentv1.RefreshCredentialsRequest{RefreshToken: first.Msg.Credentials.RefreshToken, Proof: &securityv1.SignedEnvelope{Payload: legacyPayload, Signatures: []*securityv1.Signature{{Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: ed25519.Sign(key, legacyPayload)}}}})
	_, err = client.RefreshCredentials(context.Background(), legacy)
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}

func TestCredentialRefreshV2RejectsMalformedVersionAndBindings(t *testing.T) {
	for _, kind := range []string{"unknown-protocol", "invalid-protocol", "wrong-agent", "wrong-token", "expired", "future", "missing-request", "unknown-field", "wrong-generation"} {
		t.Run(kind, func(t *testing.T) {
			record, token, key, client := refreshFixture(t)
			payload := map[string]any{"protocol": 2, "agent_id": record.Client, "refresh_token": token, "request_id": uuid.NewString(), "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "nonce": uuid.NewString()}
			switch kind {
			case "unknown-protocol":
				payload["protocol"] = 3
			case "invalid-protocol":
				payload["protocol"] = "2"
			case "wrong-agent":
				payload["agent_id"] = uuid.NewString()
			case "wrong-token":
				payload["refresh_token"] = "incorrect"
			case "expired":
				payload["timestamp"] = time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano)
			case "future":
				payload["timestamp"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
			case "missing-request":
				delete(payload, "request_id")
			case "unknown-field":
				payload["unexpected"] = true
			case "wrong-generation":
				payload["generation"] = 99
			}
			raw, err := json.Marshal(payload)
			require.NoError(t, err)
			_, err = client.RefreshCredentials(context.Background(), connect.NewRequest(&enrollmentv1.RefreshCredentialsRequest{RefreshToken: token, Proof: &securityv1.SignedEnvelope{Payload: raw, Signatures: []*securityv1.Signature{{Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: ed25519.Sign(key, raw)}}}}))
			expected := connect.CodeUnauthenticated
			if kind == "wrong-generation" {
				expected = connect.CodeAborted
			}
			require.Equal(t, expected, connect.CodeOf(err))
			var current models.AgentCredential
			require.NoError(t, dbcore.GetDBInstance().First(&current, "client = ?", record.Client).Error)
			require.Zero(t, current.Generation)
			require.Equal(t, record.RefreshTokenHash, current.RefreshTokenHash)
			require.Nil(t, current.RevokedAt)
		})
	}
}
