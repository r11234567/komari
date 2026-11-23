package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	flags.DatabaseType = flags.DatabaseTypeSQLite
	flags.DatabaseFile = "file:web_api_token_auth_test?mode=memory&cache=shared"
	db := dbcore.GetDBInstance()
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	os.Exit(m.Run())
}

func sha256Hex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func TestAPIAuthenticationMiddlewareRateLimitsUnknownBeforeSQLAndIsolatesLanes(t *testing.T) {
	t.Setenv("KOMARI_SECRET_KEY", "api-token-test-key-not-for-deployment")
	gin.SetMode(gin.TestMode)
	db := dbcore.GetDBInstance()
	now := time.Now().UTC()

	activeClient := "agent-active-" + uuid.NewString()[:8]
	activeAT := "active-at-" + uuid.NewString()
	expiredClient := "agent-expired-" + uuid.NewString()[:8]
	expiredAT := "expired-at-" + uuid.NewString()
	expEnrollment := now.Add(24 * time.Hour)
	for _, row := range []models.AgentCredential{
		{
			Client:              activeClient,
			LifecycleVersion:    1,
			AccessTokenHash:     sha256Hex(activeAT),
			RefreshTokenHash:    sha256Hex("rt-" + activeClient),
			AccessExpiresAt:     now.Add(time.Hour),
			RefreshExpiresAt:    now.Add(24 * time.Hour),
			EnrollmentExpiresAt: &expEnrollment,
			AgentPublicKey:      "pk-active",
		},
		{
			Client:              expiredClient,
			LifecycleVersion:    1,
			AccessTokenHash:     sha256Hex(expiredAT),
			RefreshTokenHash:    sha256Hex("rt-" + expiredClient),
			AccessExpiresAt:     now.Add(-time.Hour),
			RefreshExpiresAt:    now.Add(24 * time.Hour),
			EnrollmentExpiresAt: &expEnrollment,
			AgentPublicKey:      "pk-expired",
		},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		db.Where("client IN ?", []string{activeClient, expiredClient}).Delete(&models.AgentCredential{})
	})

	var sqlQueries atomic.Int64
	const callbackName = "test:count_agent_credential_queries"
	if err := db.Callback().Query().Before("gorm:query").Register(callbackName, func(_ *gorm.DB) {
		sqlQueries.Add(1)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove(callbackName)
	})

	r := gin.New()
	r.Use(APIAuthenticationMiddleware(func(_, path string) bool { return path == "/plugin-probe" }), IdentityMiddleware())
	r.GET("/plugin-probe", func(c *gin.Context) {
		if GetPrincipal(c).Type != rpc.PrincipalAnonymous || c.GetHeader("Authorization") == "" {
			c.Status(http.StatusUnauthorized)
			return
		}
		c.Status(http.StatusNoContent)
	})
	r.GET("/api/probe", func(c *gin.Context) {
		p := GetPrincipal(c)
		if p != nil && p.Type == rpc.PrincipalAgent {
			c.String(http.StatusOK, p.ClientUUID)
			return
		}
		c.Status(http.StatusUnauthorized)
	})

	sendWithIP := func(ip, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/probe", nil)
		req.RemoteAddr = ip + ":44321"
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// (a) Saturate unknown:<natIP> using repeated & short guesses -> hits 429 with 0 DB queries after first miss.
	const natIP = "198.51.100.20"
	const repeatedUnknown = "repeated-unknown-token-guess-012345"
	if rec := sendWithIP(natIP, repeatedUnknown); rec.Code != http.StatusUnauthorized {
		t.Fatalf("initial unknown failure = %d, want 401", rec.Code)
	}
	sqlQueries.Store(0)
	for i := 0; i < 9; i++ {
		tok := repeatedUnknown
		if i%2 == 1 {
			tok = "short-key"
		}
		if rec := sendWithIP(natIP, tok); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unknown failure %d = %d, want 401", i+2, rec.Code)
		}
	}
	for i := 0; i < 5; i++ {
		rec := sendWithIP(natIP, repeatedUnknown)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("saturated repeated unknown %d = %d, want 429", i+1, rec.Code)
		}
	}
	if q := sqlQueries.Load(); q != 0 {
		t.Fatalf("expected 0 DB queries for negative-cached/short 429 guesses, executed %d", q)
	}

	// (b) An uncached valid Agent token from that SAME NAT IP still succeeds (200 OK).
	activeRec := sendWithIP(natIP, activeAT)
	if activeRec.Code != http.StatusOK || activeRec.Body.String() != activeClient {
		t.Fatalf("uncached valid agent behind noisy NAT = %d %q, want 200 %q", activeRec.Code, activeRec.Body.String(), activeClient)
	}
	sqlQueries.Store(0)
	foreign := httptest.NewRequest(http.MethodGet, "/plugin-probe", nil)
	foreign.Header.Set("Authorization", "Bearer external-provider-credential")
	foreignResponse := httptest.NewRecorder()
	r.ServeHTTP(foreignResponse, foreign)
	if foreignResponse.Code != http.StatusNoContent || sqlQueries.Load() != 0 {
		t.Fatalf("foreign plugin bearer = %d with %d auth queries", foreignResponse.Code, sqlQueries.Load())
	}

	// (c) Sustained flood of >20 distinct random guesses exhausts unknownMissProbeBurst and serves 429 with 0 DB queries.
	const distinctFloodIP = "198.51.100.22"
	for i := 0; i < 20; i++ {
		rec := sendWithIP(distinctFloodIP, "distinct-unknown-"+uuid.NewString())
		if i < 10 && rec.Code != http.StatusUnauthorized {
			t.Fatalf("distinct miss %d = %d, want 401", i+1, rec.Code)
		}
		if i >= 10 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("distinct miss %d = %d, want 429", i+1, rec.Code)
		}
	}
	sqlQueries.Store(0)
	for i := 0; i < 5; i++ {
		rec := sendWithIP(distinctFloodIP, "distinct-after-cap-"+uuid.NewString())
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("distinct flood after probe cap %d = %d, want 429", i+1, rec.Code)
		}
	}
	if q := sqlQueries.Load(); q != 0 {
		t.Fatalf("expected 0 DB queries once distinct-miss probe budget exhausted, executed %d", q)
	}
	if rec := sendWithIP(distinctFloodIP, activeAT); rec.Code != http.StatusOK || rec.Body.String() != activeClient {
		t.Fatalf("pre-upgrade agent after probe exhaustion = %d %q", rec.Code, rec.Body.String())
	}

	// The middleware already has its startup snapshot. A token issued by a
	// subsequent rotation also bypasses the exhausted unknown-source budget.
	refreshToken := "rt-" + activeClient
	credential, err := enrollment.LookupRefresh(context.Background(), refreshToken)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := enrollment.RotateVerified(context.Background(), credential, refreshToken, uuid.NewString(), nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if rec := sendWithIP(distinctFloodIP, issued.AccessToken); rec.Code != http.StatusOK || rec.Body.String() != activeClient {
		t.Fatalf("newly issued agent after probe exhaustion = %d %q", rec.Code, rec.Body.String())
	}
	if err := enrollment.Revoke(activeClient, "test revocation"); err != nil {
		t.Fatal(err)
	}
	if rec := sendWithIP(distinctFloodIP, issued.AccessToken); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked agent after probe exhaustion = %d, want 401", rec.Code)
	}

	// (d) Expired Agent credentials hit 1 SQL query and spend agent: rather than unknown:.
	const expiredIP = "198.51.100.21"
	sqlQueries.Store(0)
	for i := 0; i < 10; i++ {
		if rec := sendWithIP(expiredIP, expiredAT); rec.Code != http.StatusUnauthorized {
			t.Fatalf("expired agent failure %d = %d, want 401", i+1, rec.Code)
		}
	}
	if q := sqlQueries.Load(); q != 10 {
		t.Fatalf("expected 10 single-query lookups for 10 expired agent requests, executed %d", q)
	}
	if rec := sendWithIP(expiredIP, expiredAT); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated agent lane = %d, want 429", rec.Code)
	}
	if rec := sendWithIP(expiredIP, "unknown-other-lane-"+uuid.NewString()); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown lane should remain unblocked by agent failures, got %d", rec.Code)
	}
}
