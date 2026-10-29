package connectapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialaction"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/api/admin"
	"github.com/stretchr/testify/require"
)

func TestCredentialLifecycleAdminFreshScopeReplayAndPrincipalBoundaries(t *testing.T) {
	for _, kind := range []string{"missing-proof", "wrong-resource", "api-key", "anonymous", "policy-success", "reapprove-success"} {
		t.Run(kind, func(t *testing.T) {
			ctx, user, session := actionFixture(t)
			principal := rpc.NewUserPrincipal(user)
			if kind == "api-key" {
				principal = rpc.NewAPIKeyPrincipal()
			}
			if kind == "anonymous" {
				principal = rpc.NewAnonymousPrincipal()
			}
			path := "/api/admin/agent-credentials/policy"
			policy, err := enrollment.CredentialPolicy(ctx)
			require.NoError(t, err)
			body, err := json.Marshal(policy)
			require.NoError(t, err)
			if kind == "reapprove-success" {
				record, _, _, _ := refreshFixture(t)
				path = "/api/admin/agent-credentials/" + record.Client + "/reapprove"
				rows, _, err := enrollment.CredentialStatuses(ctx, 1)
				require.NoError(t, err)
				var fingerprint string
				for _, row := range rows {
					if row.AgentID == record.Client {
						fingerprint = row.Fingerprint
					}
				}
				require.Len(t, fingerprint, 43)
				body, err = json.Marshal(map[string]any{"generation": record.Generation, "fingerprint": fingerprint})
				require.NoError(t, err)
			}
			token := ""
			if kind != "missing-proof" && kind != "anonymous" && kind != "api-key" {
				resource := credentialaction.RESTResource("POST", path)
				if kind == "wrong-resource" {
					resource = credentialaction.RESTResource("POST", "/api/admin/other")
				}
				token = actionAuthorization(t, ctx, credential.SensitiveOperation, resource)
			}
			router := gin.New()
			router.Use(func(c *gin.Context) { api.SetPrincipal(c, principal); c.Set("uuid", user); c.Next() })
			admin.RegisterCredentialLifecycleRoutes(router.Group("/api/admin"))
			send := func(method, path string, body []byte) *httptest.ResponseRecorder {
				request := httptest.NewRequest(method, path, bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(credentialaction.Header, token)
				request.AddCookie(&http.Cookie{Name: "session_token", Value: session})
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				return response
			}
			response := send("POST", path, body)
			expected := 401
			if kind == "api-key" || kind == "anonymous" {
				expected = 403
			}
			if kind == "policy-success" || kind == "reapprove-success" {
				expected = 200
			}
			require.Equal(t, expected, response.Code, response.Body.String())
			if expected == 200 {
				replay := send("POST", path, body)
				require.Equal(t, 401, replay.Code, "fresh proof must be single use")
				result := send("GET", "/api/admin/agent-credentials", nil)
				require.Equal(t, 200, result.Code)
				for _, private := range []string{"refresh_token", "access_token", "refresh_recovery_sealed", "agent_public_key"} {
					require.NotContains(t, result.Body.String(), private)
				}
			}
		})
	}
}

func TestCredentialLifecycleExpiredAuthorizationRejectsAccessToken(t *testing.T) {
	record, token, _, _ := refreshFixture(t)
	past := time.Now().Add(-time.Second)
	require.NoError(t, dbcore.GetDBInstance().Model(&record).Updates(map[string]any{"lifecycle_version": 1, "enrollment_expires_at": past}).Error)
	_, err := enrollment.ResolveAccessToken("synthetic-access" + token)
	require.Error(t, err)
	var current models.AgentCredential
	require.NoError(t, dbcore.GetDBInstance().First(&current, "client = ?", record.Client).Error)
	require.Nil(t, current.RevokedAt, "ordinary expiry is not compromise revocation")
}
