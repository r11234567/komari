package connectapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/apitokens"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialaction"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestApiTokenCreationFreshProofCannotBeReplayedOrBroadened(t *testing.T) {
	for _, kind := range []string{"missing", "wrong-scope", "wrong-node", "wrong-expiry", "api-key", "success-and-replay"} {
		t.Run(kind, func(t *testing.T) {
			ctx, _, _ := actionFixture(t)
			agent, _, err := clients.CreateClient()
			require.NoError(t, err)
			req := &adminv1.CreateApiTokenRequest{Name: "reader", Scopes: []string{rpc.ScopeNodesRead}, AgentIds: []string{agent}, ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}
			if kind != "missing" && kind != "api-key" {
				resource := credentialaction.ScopedResource("api-token:create", apiTokenCreateTargets(req)...)
				token := actionAuthorization(t, ctx, credential.SensitiveOperation, resource)
				req.TwoFactor = &commonv1.TwoFactorProof{AuthorizationToken: token}
			}
			switch kind {
			case "wrong-scope":
				req.Scopes = append(req.Scopes, rpc.ScopeMetricsRead)
			case "wrong-node":
				req.AgentIds = nil
				req.AllAgents = true
			case "wrong-expiry":
				req.ExpiresAt = timestamppb.New(time.Now().Add(2 * time.Hour))
			case "api-key":
				ctx = rpc.NewContextWithMeta(ctx, &rpc.ContextMeta{Principal: rpc.NewAPIKeyPrincipal()})
			}
			service := &apiTokenService{}
			result, err := service.CreateApiToken(ctx, connect.NewRequest(req))
			if kind != "success-and-replay" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, result.Msg.Secret)
			require.Equal(t, "no-store", result.Header().Get("Cache-Control"))
			_, err = service.CreateApiToken(ctx, connect.NewRequest(req))
			require.Error(t, err)
			listed, err := service.ListApiTokens(ctx, connect.NewRequest(&adminv1.ListApiTokensRequest{}))
			require.NoError(t, err)
			serialized, err := json.Marshal(listed.Msg)
			require.NoError(t, err)
			require.NotContains(t, string(serialized), result.Msg.Secret)
			require.NotContains(t, string(serialized), "secret_hash")
		})
	}
}

func TestApiTokenManagementRejectsNoninteractivePrincipals(t *testing.T) {
	for _, principal := range []*rpc.Principal{rpc.NewAnonymousPrincipal(), rpc.NewAgentPrincipal("node"), rpc.NewAPIKeyPrincipal(), rpc.PrincipalFromRole(rpc.RoleAdmin), rpc.NewUserPrincipal("no-session")} {
		ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: principal})
		service := &apiTokenService{}
		_, err := service.ListApiTokens(ctx, connect.NewRequest(&adminv1.ListApiTokensRequest{}))
		require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
		_, err = service.CreateApiToken(ctx, connect.NewRequest(&adminv1.CreateApiTokenRequest{}))
		require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
		_, err = service.RevokeApiToken(ctx, connect.NewRequest(&adminv1.RevokeApiTokenRequest{}))
		require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
		_, err = service.DisableLegacyApiKey(ctx, connect.NewRequest(&adminv1.DisableLegacyApiKeyRequest{}))
		require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	}
}

func TestApiTokenHTTPResourceScopeAndRevocationCannotFallBackToCookie(t *testing.T) {
	ctx, owner, session := actionFixture(t)
	a, _, err := clients.CreateClient()
	require.NoError(t, err)
	b, _, err := clients.CreateClient()
	require.NoError(t, err)
	_, secret, err := apitokens.Create(ctx, owner, apitokens.Policy{Name: "http-reader", Scopes: []string{rpc.ScopeNodesRead}, AgentIDs: []string{a}, ExpiresAt: time.Now().Add(time.Hour)}, "")
	require.NoError(t, err)
	principal, err := apitokens.Authenticate(ctx, secret, "192.0.2.8")
	require.NoError(t, err)
	router := gin.New()
	router.Use(api.APIAuthenticationMiddleware(), security.NewCorsController(true, "").Middleware(), api.IdentityMiddleware(), api.PrivateSiteMiddleware())
	Register(router)
	invoke := func(path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Connect-Protocol-Version", "1")
		request.Header.Set("Authorization", "Bearer "+secret)
		request.AddCookie(&http.Cookie{Name: "session_token", Value: session})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	response := invoke("/komari.browser.v1.BrowserService/ListAgents", "{}")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var listed struct {
		Agents []struct {
			AgentID string `json:"agentId"`
		} `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &listed))
	require.Len(t, listed.Agents, 1)
	require.Equal(t, a, listed.Agents[0].AgentID)
	response = invoke("/komari.browser.v1.BrowserService/GetAgent", `{"agentId":"`+b+`"}`)
	require.Equal(t, http.StatusForbidden, response.Code)
	for _, path := range []string{"/komari.metrics.v1.MetricsService/QueryMetrics", "/komari.admin.v1.ApiTokenService/ListApiTokens", "/komari.webssh.v1.WebSSHService/CreateSession", "/api/admin/settings"} {
		require.Equal(t, http.StatusForbidden, invoke(path, "{}").Code, path)
	}
	// Both native revoke and HTTP authentication use the durable row, not a
	// cache which would keep an old key alive after a successful revocation.
	_, err = (&apiTokenService{}).RevokeApiToken(ctx, connect.NewRequest(&adminv1.RevokeApiTokenRequest{Id: principal.APIToken.ID}))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, invoke("/komari.browser.v1.BrowserService/ListAgents", "{}").Code)
	var revoked int64
	require.NoError(t, dbcore.GetDBInstance().Table("api_tokens").Where("id = ? AND revoked_at IS NOT NULL", principal.APIToken.ID).Count(&revoked).Error)
	require.Equal(t, int64(1), revoked)
}

func TestApiTokenReductionsNeedHumanSessionAndOwnershipButNoSecondFactor(t *testing.T) {
	ctx, owner, session := actionFixture(t)
	otherCtx, _, _ := actionFixture(t)
	require.NoError(t, dbcore.GetDBInstance().Model(&models.User{}).Where("uuid = ?", owner).Update("two_factor", "").Error)
	row, secret, err := apitokens.Create(ctx, owner, apitokens.Policy{Name: "revoke-without-factor", Scopes: []string{rpc.ScopeNodesRead}, AllAgents: true, ExpiresAt: time.Now().Add(time.Hour)}, "")
	require.NoError(t, err)
	service := &apiTokenService{}
	_, err = service.RevokeApiToken(otherCtx, connect.NewRequest(&adminv1.RevokeApiTokenRequest{Id: row.ID}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
	_, err = apitokens.Authenticate(ctx, secret, "192.0.2.8")
	require.NoError(t, err)
	// Removing second-factor friction must not bypass cookie-origin protection.
	router := gin.New()
	router.Use(api.APIAuthenticationMiddleware(), security.NewCorsController(true, "").Middleware(), api.IdentityMiddleware(), api.PrivateSiteMiddleware())
	Register(router)
	request := httptest.NewRequest(http.MethodPost, "http://panel.example/komari.admin.v1.ApiTokenService/RevokeApiToken", strings.NewReader(`{"id":"`+row.ID+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Origin", "https://foreign.example")
	request.AddCookie(&http.Cookie{Name: "session_token", Value: session})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	_, err = apitokens.Authenticate(ctx, secret, "192.0.2.8")
	require.NoError(t, err)
	_, err = service.RevokeApiToken(ctx, connect.NewRequest(&adminv1.RevokeApiTokenRequest{Id: row.ID}))
	require.NoError(t, err)
	_, err = apitokens.Authenticate(ctx, secret, "192.0.2.8")
	require.Error(t, err)
	require.NoError(t, config.Set(config.ApiKeyKey, "synthetic-global-key-to-disable"))
	t.Cleanup(func() { _ = config.Set(config.ApiKeyKey, "") })
	_, err = service.DisableLegacyApiKey(ctx, connect.NewRequest(&adminv1.DisableLegacyApiKeyRequest{}))
	require.NoError(t, err)
	key, err := config.GetAs[string](config.ApiKeyKey, "")
	require.NoError(t, err)
	require.Empty(t, key)
}

func TestApiKeyFailuresDoNotLockOutEnrolledDevice(t *testing.T) {
	require.NoError(t, config.Set(config.ApiKeyKey, "synthetic-legacy-api-key-0123456789"))
	t.Cleanup(func() { _ = config.Set(config.ApiKeyKey, "") })
	router := gin.New()
	router.Use(api.APIAuthenticationMiddleware(), api.IdentityMiddleware())
	router.POST("/probe", func(c *gin.Context) { c.String(200, api.GetRole(c)) })
	send := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "/probe", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	for i := 0; i < 10; i++ {
		require.Equal(t, http.StatusUnauthorized, send("wrong-key").Code)
	}
	require.Equal(t, http.StatusTooManyRequests, send("wrong-key").Code)
	response := send(testAccessToken)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, api.RoleClient, response.Body.String())
}

func TestApiAuthenticationPreservesPluginOwnedForeignBearer(t *testing.T) {
	ctx, owner, _ := actionFixture(t)
	_, secret, err := apitokens.Create(ctx, owner, apitokens.Policy{Name: "reader", Scopes: []string{rpc.ScopeNodesRead}, AllAgents: true, ExpiresAt: time.Now().Add(time.Hour)}, "")
	require.NoError(t, err)
	router := gin.New()
	router.Use(api.APIAuthenticationMiddleware(func(method, pattern string) bool { return method == "POST" && pattern == "/plugin-own-api" }), api.IdentityMiddleware())
	router.POST("/plugin-own-api", func(c *gin.Context) {
		if c.GetHeader("Authorization") == "Bearer foreign-provider-secret" {
			c.Status(http.StatusNoContent)
			return
		}
		c.Status(http.StatusUnauthorized)
	})
	for _, test := range []struct {
		secret string
		status int
	}{{"foreign-provider-secret", http.StatusNoContent}, {secret, http.StatusForbidden}} {
		request := httptest.NewRequest("POST", "/plugin-own-api", nil)
		request.Header.Set("Authorization", "Bearer "+test.secret)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, test.status, response.Code, response.Body.String())
	}
}
