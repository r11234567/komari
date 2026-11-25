package connectapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/apitokens"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

func TestDPoPRejectsBodyOverUnaryLimit(t *testing.T) {
	agentID, accessToken, key := dpopTestAgent(t)
	body := make([]byte, maxDPoPBodyBytes+1)
	ctx := makeDPoPContext(http.MethodPost, "/komari.report.v1.AgentReportService/SubmitReport", body, accessToken, uuid.NewString(), key, false)
	require.ErrorContains(t, verifyDPoP(ctx, rpc.NewAgentPrincipal(agentID)), "request body exceeds maximum size")
}

func TestDPoPFailsClosedOnUnsignedAgentKeyLookupError(t *testing.T) {
	callback := dbcore.GetDBInstance().Callback().Query()
	const callbackName = "test:inject_agent_key_lookup_error"
	lookupErr := errors.New("synthetic database lookup failure")
	require.NoError(t, callback.Before("gorm:query").Register(callbackName, func(db *gorm.DB) {
		if _, ok := db.Statement.Model.(*models.AgentCredential); ok {
			_ = db.AddError(lookupErr)
		}
	}))
	t.Cleanup(func() { require.NoError(t, callback.Remove(callbackName)) })

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/komari.report.v1.AgentReportService/SubmitReport", nil)
	require.ErrorIs(t, verifyDPoP(ctx, rpc.NewAgentPrincipal(uuid.NewString())), lookupErr)
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

func TestDPoPNonceLengthCapAndFutureTimestampRetention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, accessToken, priv := dpopTestAgent(t)
	principal := rpc.NewAgentPrincipal(agentID)

	// Nonces exceeding maxNonceLength (128) must be rejected.
	longNonce := string(bytes.Repeat([]byte("a"), 129))
	longCtx := makeDPoPContext(http.MethodPost, "/komari.report.v1.ReportService/ReportBasicInfo", []byte(`{}`), accessToken, longNonce, priv, false)
	require.Error(t, verifyDPoP(longCtx, principal))

	// A valid future-skewed signature (ts = now + 100s) must retain its nonce
	// until after ts + 120s (well beyond a flat 120s TTL from receive time).
	futureTS := time.Now().Add(100 * time.Second).Unix()
	futureNonce := "future-nonce-" + uuid.NewString()
	body := []byte(`{"skew":true}`)
	req := httptest.NewRequest(http.MethodPost, "/komari.report.v1.ReportService/ReportBasicInfo", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	tsStr := fmt.Sprintf("%d", futureTS)
	req.Header.Set("X-Komari-Timestamp", tsStr)
	req.Header.Set("X-Komari-Nonce", futureNonce)
	bodySum := sha256.Sum256(body)
	tokSum := sha256.Sum256([]byte(accessToken))
	canonical := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		http.MethodPost, "/komari.report.v1.ReportService/ReportBasicInfo", tsStr, futureNonce,
		hex.EncodeToString(tokSum[:]), hex.EncodeToString(bodySum[:]))
	req.Header.Set("X-Komari-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(canonical))))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	require.NoError(t, verifyDPoP(c, principal))

	key := agentID + "\x00" + futureNonce
	dpopReplayCache.mu.Lock()
	entry, exists := dpopReplayCache.entries[key]
	dpopReplayCache.mu.Unlock()
	require.True(t, exists)
	require.True(t, entry.expires.After(time.Unix(futureTS, 0).Add(120*time.Second)),
		"nonce expiry %s must cover full valid signature window beyond %s", entry.expires, time.Unix(futureTS, 0).Add(120*time.Second))
}

func makeRFC9449Proof(t *testing.T, priv ed25519.PrivateKey, pubKey ed25519.PublicKey, method, htu, accessToken, nonce, jti string, body []byte, bm string) string {
	t.Helper()
	hdrJSON, err := json.Marshal(dpopJWTHeader{
		Typ: "dpop+jwt",
		Alg: "EdDSA",
		JWK: dpopJWTJWK{
			Kty: "OKP",
			Crv: "Ed25519",
			X:   base64.RawURLEncoding.EncodeToString(pubKey),
		},
	})
	require.NoError(t, err)
	tokSum := sha256.Sum256([]byte(accessToken))
	bodySum := sha256.Sum256(body)
	claimsJSON, err := json.Marshal(dpopJWTClaims{
		JTI:   jti,
		HTM:   method,
		HTU:   htu,
		IAT:   time.Now().Unix(),
		ATH:   base64.RawURLEncoding.EncodeToString(tokSum[:]),
		Nonce: nonce,
		BH:    base64.RawURLEncoding.EncodeToString(bodySum[:]),
		BM:    bm,
	})
	require.NoError(t, err)
	input := base64.RawURLEncoding.EncodeToString(hdrJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	sig := ed25519.Sign(priv, []byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func makeRFC9449Context(t *testing.T, method, rawURL string, body []byte, authHeader, proof string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, rawURL, bytes.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if proof != "" {
		req.Header.Set("DPoP", proof)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	return c, rec
}

func TestRFC9449DPoPNonceChallengeReplayAndBindings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, accessToken, priv := dpopTestAgent(t)
	pub := priv.Public().(ed25519.PublicKey)
	principal := rpc.NewAgentPrincipal(agentID)
	body := []byte(`{"metric":"cpu"}`)
	unaryURL := "http://example.com/komari.report.v1.AgentReportService/SubmitReport"

	// 1. Cold-start unary request without server nonce fails with use_dpop_nonce and issues DPoP-Nonce without consuming jti.
	coldJTI := "jti-" + uuid.NewString()
	coldProof := makeRFC9449Proof(t, priv, pub, http.MethodPost, unaryURL, accessToken, "", coldJTI, body, "")
	coldCtx, coldRec := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken, coldProof)
	err := verifyDPoP(coldCtx, principal)
	require.Error(t, err)
	issuedNonce := coldRec.Header().Get("DPoP-Nonce")
	require.NotEmpty(t, issuedNonce)
	require.Contains(t, coldRec.Header().Get("WWW-Authenticate"), `use_dpop_nonce`)

	// Retrying with the SAME jti and the issued server nonce succeeds once, then replay fails.
	okProof := makeRFC9449Proof(t, priv, pub, http.MethodPost, unaryURL, accessToken, issuedNonce, coldJTI, body, "")
	okCtx, okRec := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken, okProof)
	require.NoError(t, verifyDPoP(okCtx, principal))
	require.NotEmpty(t, okRec.Header().Get("DPoP-Nonce"))
	var credential models.AgentCredential
	require.NoError(t, dbcore.GetDBInstance().Where("client = ?", agentID).First(&credential).Error)
	require.True(t, credential.DPoPJWTRequired)
	legacyCtx := makeDPoPContext(http.MethodPost, "/komari.report.v1.AgentReportService/SubmitReport", body, accessToken, uuid.NewString(), priv, false)
	require.ErrorContains(t, verifyDPoP(legacyCtx, principal), "must use a JWT proof")
	duplicateCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken, okProof)
	duplicateCtx.Request.Header.Add("DPoP", okProof)
	require.ErrorContains(t, verifyDPoP(duplicateCtx, principal), "multiple proof values")

	replayCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken, okProof)
	require.Error(t, verifyDPoP(replayCtx, principal))

	// 2. Reject wrong Authorization scheme ("Bearer" when sending DPoP proof), wrong htu, htm, ath, bh, jwk, and spoofed bm="stream".
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	otherPub := otherPriv.Public().(ed25519.PublicKey)

	badSchemeCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "Bearer "+accessToken,
		makeRFC9449Proof(t, priv, pub, http.MethodPost, unaryURL, accessToken, issuedNonce, "jti-"+uuid.NewString(), body, ""))
	require.Error(t, verifyDPoP(badSchemeCtx, principal))

	badHTUCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken,
		makeRFC9449Proof(t, priv, pub, http.MethodPost, "http://evil.example/komari.report.v1.AgentReportService/SubmitReport", accessToken, issuedNonce, "jti-"+uuid.NewString(), body, ""))
	require.Error(t, verifyDPoP(badHTUCtx, principal))

	badHTMCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken,
		makeRFC9449Proof(t, priv, pub, http.MethodGet, unaryURL, accessToken, issuedNonce, "jti-"+uuid.NewString(), body, ""))
	require.Error(t, verifyDPoP(badHTMCtx, principal))

	badATHCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken,
		makeRFC9449Proof(t, priv, pub, http.MethodPost, unaryURL, "different-token", issuedNonce, "jti-"+uuid.NewString(), body, ""))
	require.Error(t, verifyDPoP(badATHCtx, principal))

	badBHCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken,
		makeRFC9449Proof(t, priv, pub, http.MethodPost, unaryURL, accessToken, issuedNonce, "jti-"+uuid.NewString(), []byte("tampered"), ""))
	require.Error(t, verifyDPoP(badBHCtx, principal))

	badJWKCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken,
		makeRFC9449Proof(t, otherPriv, otherPub, http.MethodPost, unaryURL, accessToken, issuedNonce, "jti-"+uuid.NewString(), body, ""))
	require.Error(t, verifyDPoP(badJWKCtx, principal))

	spoofStreamCtx, _ := makeRFC9449Context(t, http.MethodPost, unaryURL, body, "DPoP "+accessToken,
		makeRFC9449Proof(t, priv, pub, http.MethodPost, unaryURL, accessToken, issuedNonce, "jti-"+uuid.NewString(), nil, "stream"))
	require.Error(t, verifyDPoP(spoofStreamCtx, principal))

	// Client streams use the same nonce requirement, including on cold start.
	streamURL := "http://example.com/komari.file.v1.FileService/AttachFileOperations"
	coldStreamProof := makeRFC9449Proof(t, priv, pub, http.MethodPost, streamURL, accessToken, "", "jti-"+uuid.NewString(), nil, "stream")
	streamCtx, streamRec := makeRFC9449Context(t, http.MethodPost, streamURL, []byte("wire-frames"), "DPoP "+accessToken, coldStreamProof)
	require.Error(t, verifyDPoP(streamCtx, principal))
	require.Contains(t, streamRec.Header().Get("WWW-Authenticate"), "use_dpop_nonce")
	streamNonce := streamRec.Header().Get("DPoP-Nonce")
	require.NotEmpty(t, streamNonce)
	validStreamProof := makeRFC9449Proof(t, priv, pub, http.MethodPost, streamURL, accessToken, streamNonce, "jti-"+uuid.NewString(), nil, "stream")
	validStreamCtx, _ := makeRFC9449Context(t, http.MethodPost, streamURL, []byte("wire-frames"), "DPoP "+accessToken, validStreamProof)
	require.NoError(t, verifyDPoP(validStreamCtx, principal))

	forgedStreamProof := makeRFC9449Proof(t, priv, pub, http.MethodPost, streamURL, accessToken, "0.forgedmac", "jti-"+uuid.NewString(), nil, "stream")
	forgedStreamCtx, _ := makeRFC9449Context(t, http.MethodPost, streamURL, []byte("wire-frames"), "DPoP "+accessToken, forgedStreamProof)
	require.Error(t, verifyDPoP(forgedStreamCtx, principal))
}

func TestDPoPMiddlewareAndBridgeAuthSchemes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, accessToken, priv := dpopTestAgent(t)
	pub := priv.Public().(ed25519.PublicKey)

	ownerID := uuid.NewString()
	require.NoError(t, dbcore.GetDBInstance().Create(&models.User{
		UUID:     ownerID,
		Username: ownerID,
		Passwd:   "synthetic-unused",
	}).Error)
	t.Cleanup(func() {
		dbcore.GetDBInstance().Where("uuid = ?", ownerID).Delete(&models.User{})
	})

	createdRow, createdSecret, err := apitokens.Create(t.Context(), ownerID, apitokens.Policy{
		Name:      "dpop-pat-test",
		Scopes:    []string{rpc.ScopeNodesRead},
		AllAgents: true,
		ExpiresAt: time.Now().Add(time.Hour),
	}, "")
	require.NoError(t, err)
	t.Cleanup(func() {
		dbcore.GetDBInstance().Where("id = ?", createdRow.ID).Delete(&models.APIToken{})
	})

	router := gin.New()
	router.Use(api.APIAuthenticationMiddleware())
	router.POST("/api/clients/report", func(c *gin.Context) {
		t.Error("a DPoP credential was admitted to a route without proof verification")
		c.Status(http.StatusOK)
	})
	router.POST("/komari.report.v1.AgentReportService/SubmitReport", bridge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := rpc.MetaFromContext(r.Context())
		if meta == nil || meta.Principal == nil || meta.Principal.ClientUUID != agentID {
			http.Error(w, "missing agent principal", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), nil))

	targetPath := "/komari.report.v1.AgentReportService/SubmitReport"
	targetURL := "http://example.com" + targetPath
	body := []byte(`{}`)

	// 1. Cold-start Authorization: DPoP reaches bridge -> 401 use_dpop_nonce with DPoP-Nonce header.
	req1 := httptest.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	req1.Header.Set("Authorization", "DPoP "+accessToken)
	req1.Header.Set("DPoP", makeRFC9449Proof(t, priv, pub, http.MethodPost, targetURL, accessToken, "", "jti-"+uuid.NewString(), body, ""))
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusUnauthorized, rec1.Code)
	nonce := rec1.Header().Get("DPoP-Nonce")
	require.NotEmpty(t, nonce)
	require.Contains(t, rec1.Header().Get("WWW-Authenticate"), `use_dpop_nonce`)

	// 2. Retried request with returned DPoP-Nonce succeeds with authenticated agent principal.
	req2 := httptest.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	req2.Header.Set("Authorization", "DPoP "+accessToken)
	req2.Header.Set("DPoP", makeRFC9449Proof(t, priv, pub, http.MethodPost, targetURL, accessToken, nonce, "jti-"+uuid.NewString(), body, ""))
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)

	// 3. Invalid DPoP agent token is rejected at APIAuthenticationMiddleware without falling back to cookies.
	req3 := httptest.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	req3.Header.Set("Authorization", "DPoP invalid-agent-token")
	req3.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: "forged-session"})
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	require.Equal(t, http.StatusUnauthorized, rec3.Code)

	// 4. User PAT sent with Authorization: DPoP is rejected (DPoP is agent-only).
	req4 := httptest.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	req4.Header.Set("Authorization", "DPoP "+createdSecret)
	rec4 := httptest.NewRecorder()
	router.ServeHTTP(rec4, req4)
	require.Equal(t, http.StatusUnauthorized, rec4.Code)

	report := httptest.NewRequest(http.MethodPost, "/api/clients/report", nil)
	report.Header.Set("Authorization", "DPoP "+accessToken)
	reportResponse := httptest.NewRecorder()
	router.ServeHTTP(reportResponse, report)
	require.Equal(t, http.StatusUnauthorized, reportResponse.Code)
}
