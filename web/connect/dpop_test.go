package connectapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"github.com/stretchr/testify/require"
)

func dpopTestAgent(t *testing.T) (string, string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	agentID := uuid.NewString()
	accessToken := "dpop-access-" + uuid.NewString()
	accessHash := sha256.Sum256([]byte(accessToken))
	refreshHash := sha256.Sum256([]byte("dpop-refresh-" + uuid.NewString()))
	row := models.AgentCredential{
		Client:           agentID,
		AccessTokenHash:  hex.EncodeToString(accessHash[:]),
		RefreshTokenHash: hex.EncodeToString(refreshHash[:]),
		AccessExpiresAt:  time.Now().Add(time.Hour),
		RefreshExpiresAt: time.Now().Add(24 * time.Hour),
		AgentPublicKey:   base64.StdEncoding.EncodeToString(pub),
		Scopes:           "[]",
	}
	require.NoError(t, dbcore.GetDBInstance().Create(&row).Error)
	t.Cleanup(func() {
		dbcore.GetDBInstance().Where("client = ?", agentID).Delete(&models.AgentCredential{})
	})
	return agentID, accessToken, priv
}

func makeDPoPContext(method, path string, body []byte, accessToken, nonce string, priv ed25519.PrivateKey, streamHeader bool) *gin.Context {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	req.Header.Set("X-Komari-Timestamp", ts)
	req.Header.Set("X-Komari-Nonce", nonce)
	if streamHeader {
		req.Header.Set("X-Komari-Body-Mode", "stream")
	}
	var payloadHash string
	if streamHeader {
		sum := sha256.Sum256(nil)
		payloadHash = hex.EncodeToString(sum[:])
	} else {
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}
	tokenHash := sha256.Sum256([]byte(accessToken))
	canonical := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		method, path, ts, nonce, hex.EncodeToString(tokenHash[:]), payloadHash)
	sig := ed25519.Sign(priv, []byte(canonical))
	req.Header.Set("X-Komari-Signature", base64.StdEncoding.EncodeToString(sig))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return c
}

func TestDPoPVerifyBeforeConsumeAndAuthoritativeStreamingMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, accessToken, priv := dpopTestAgent(t)
	principal := rpc.NewAgentPrincipal(agentID)

	// D1: Invalid signature must not consume the nonce.
	nonce := "nonce-" + uuid.NewString()
	badCtx := makeDPoPContext(http.MethodPost, "/komari.report.v1.ReportService/ReportBasicInfo", []byte(`{"a":1}`), accessToken, nonce, priv, false)
	badCtx.Request.Header.Set("X-Komari-Signature", base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
	require.Error(t, verifyDPoP(badCtx, principal))

	// Legitimate request reusing that unconsumed nonce must succeed once.
	goodCtx := makeDPoPContext(http.MethodPost, "/komari.report.v1.ReportService/ReportBasicInfo", []byte(`{"a":1}`), accessToken, nonce, priv, false)
	require.NoError(t, verifyDPoP(goodCtx, principal))

	// Replaying the valid request must now be rejected.
	replayCtx := makeDPoPContext(http.MethodPost, "/komari.report.v1.ReportService/ReportBasicInfo", []byte(`{"a":1}`), accessToken, nonce, priv, false)
	require.Error(t, verifyDPoP(replayCtx, principal))

	// A2.2: Spoofing X-Komari-Body-Mode: stream on a unary RPC with non-empty body must be rejected.
	spoofCtx := makeDPoPContext(http.MethodPost, "/komari.report.v1.ReportService/ReportBasicInfo", []byte(`{"a":1}`), accessToken, "nonce-"+uuid.NewString(), priv, true)
	require.Error(t, verifyDPoP(spoofCtx, principal))

	// Genuine client-streaming procedure accepts stream mode (empty body hash).
	streamCtx := makeDPoPContext(http.MethodPost, "/komari.webssh.v1.WebSSHService/AttachSession", []byte("stream-frame"), accessToken, "nonce-"+uuid.NewString(), priv, true)
	require.NoError(t, verifyDPoP(streamCtx, principal))
}

func TestDPoPFailsClosedOnCorruptedPublicKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, accessToken, priv := dpopTestAgent(t)
	require.NoError(t, dbcore.GetDBInstance().Model(&models.AgentCredential{}).Where("client = ?", agentID).Update("agent_public_key", "not-valid-base64!!!").Error)

	ctx := makeDPoPContext(http.MethodPost, "/komari.report.v1.ReportService/ReportBasicInfo", []byte(`{}`), accessToken, "nonce-"+uuid.NewString(), priv, false)
	require.Error(t, verifyDPoP(ctx, rpc.NewAgentPrincipal(agentID)))
}

func TestVerifyRefreshProofRejectsPermissiveFallbacks(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	agentID := uuid.NewString()
	rt := "rt-" + uuid.NewString()

	// Permissive JSON payload without canonical 4-line format must be rejected.
	jsonPayload := []byte(fmt.Sprintf(`{"agent_id":%q,"refresh_token":%q}`, agentID, rt))
	err = verifyRefreshProof(&securityv1.SignedEnvelope{
		Payload:    jsonPayload,
		Signatures: []*securityv1.Signature{{Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: ed25519.Sign(priv, jsonPayload)}},
	}, pubB64, agentID, rt)
	require.Error(t, err)

	// Substring fallback payload must be rejected.
	rawPayload := []byte("prefix " + agentID + " middle " + rt + " suffix")
	err = verifyRefreshProof(&securityv1.SignedEnvelope{
		Payload:    rawPayload,
		Signatures: []*securityv1.Signature{{Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: ed25519.Sign(priv, rawPayload)}},
	}, pubB64, agentID, rt)
	require.Error(t, err)

	// Canonical 4-line komari-refresh-v1 passes once and rejects replay.
	validPayload := []byte(fmt.Sprintf("komari-refresh-v1\n%s\n%s\n%s", agentID, rt, time.Now().UTC().Format(time.RFC3339)))
	env := &securityv1.SignedEnvelope{
		Payload:    validPayload,
		Signatures: []*securityv1.Signature{{Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: ed25519.Sign(priv, validPayload)}},
	}
	require.NoError(t, verifyRefreshProof(env, pubB64, agentID, rt))
	require.Error(t, verifyRefreshProof(env, pubB64, agentID, rt))
}
