package connectapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialaction"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/api/admin"
	"github.com/komari-monitor/komari/web/signonauth"
	"github.com/komari-monitor/komari/web/workspace"
	"github.com/pquerna/otp/totp"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	workspacev1 "github.com/r11234567/komari-proto/gen/go/komari/workspace/v1"
)

// Isolated synthetic accounts only. No notification transport or host command
// is invoked; this verifies consumers use the gateway's completed grant.
func actionFixture(t *testing.T) (context.Context, string, string) {
	t.Helper()
	user, session := uuid.NewString(), "synthetic-action-"+uuid.NewString()
	db := dbcore.GetDBInstance()
	if err := db.Create(&models.User{UUID: user, Username: user, Passwd: "synthetic-unused", TwoFactor: "JBSWY3DPEHPK3PXP"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Session{UUID: user, Session: session, Expires: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		credentialgateway.Default.RevokeUser(user)
		db.Where("uuid = ?", user).Delete(&models.Session{})
		db.Where("uuid = ?", user).Delete(&models.User{})
	})
	return rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal(user), UserUUID: user, SessionToken: session}), user, session
}

func actionAuthorization(t *testing.T, ctx context.Context, purpose credential.Purpose, resource string) string {
	t.Helper()
	c, err := credentialgateway.Default.Begin(ctx, credential.Request{ProviderID: "native:totp", Purpose: purpose, Resource: resource})
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]string{"code": code})
	a, err := credentialgateway.Default.Complete(ctx, c.ID, proof)
	if err != nil {
		t.Fatal(err)
	}
	return a.Token
}

func TestTrustedActionRESTConsumers(t *testing.T) {
	for _, name := range []string{"account", "sign-on", "password", "disable-totp"} {
		t.Run(name, func(t *testing.T) {
			ctx, user, session := actionFixture(t)
			purpose, resource, path := credential.SensitiveOperation, credentialaction.AccountEntry, "/api/admin/2fa/verify"
			body := []byte(`{}`)
			var handler gin.HandlerFunc = admin.Verify2FA
			switch name {
			case "sign-on":
				purpose, resource, path, handler = credential.LoginSettings, credentialaction.LoginSettings, "/api/admin/2fa/sign-on", admin.UnlockSignOn
			case "password":
				resource, path, handler = credentialaction.PasswordResource(user), "/api/admin/update/user", admin.UpdateUser
				body, _ = json.Marshal(map[string]string{"uuid": user, "password": "Synthetic-new-password-123"})
			case "disable-totp":
				resource, path, handler = credentialaction.RESTResource("POST", "/api/admin/2fa/disable"), "/api/admin/2fa/disable", admin.Disable2FA
			}
			token := actionAuthorization(t, ctx, purpose, resource)
			router := gin.New()
			router.Use(func(c *gin.Context) { api.SetPrincipal(c, rpc.NewUserPrincipal(user)); c.Set("uuid", user); c.Next() })
			if name == "password" {
				router.POST(path, handler)
			} else {
				router.POST(path, api.RequireSensitive2FA(), handler)
			}
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(credentialaction.Header, token)
			req.AddCookie(&http.Cookie{Name: "session_token", Value: session})
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("action failed: %d %s", rec.Code, rec.Body.String())
			}
			if name == "sign-on" {
				var cookie *http.Cookie
				for _, item := range rec.Result().Cookies() {
					if item.Name == signonauth.CookieName {
						cookie = item
					}
				}
				if cookie == nil || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
					t.Fatal("missing protected sign-on cookie")
				}
				if err := signonauth.Require(signonauth.WithToken(ctx, cookie.Value)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTrustedActionWorkspaceUnlock(t *testing.T) {
	ctx, user, session := actionFixture(t)
	agent, _, err := clients.CreateClient()
	if err != nil {
		t.Fatal(err)
	}
	db := dbcore.GetDBInstance()
	if err := db.Create(&models.ClientPrivilegedRevision{Client: agent, Revision: 1, Config: `{"remote_control_enabled":true,"webssh_enabled":true}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ClientPrivilegedState{Client: agent, ProtocolVersion: 2, AppliedRevision: 1, DesiredRevision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	token := actionAuthorization(t, ctx, credential.UnlockWorkspace, credentialaction.WorkspaceResource(agent, "alice"))
	response, err := (&workspaceService{}).UnlockWorkspace(ctx, connect.NewRequest(&workspacev1.UnlockWorkspaceRequest{AgentId: agent, SystemUser: "alice", TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token}}))
	if err != nil {
		t.Fatal(err)
	}
	grant := response.Msg.Authorization
	t.Cleanup(func() { _ = workspace.Default.Lock(grant.GrantToken, user, session) })
	if grant.SystemUser != "alice" || grant.AgentId != agent {
		t.Fatal("workspace lost verified target")
	}
	if _, err := workspace.Default.Check(grant.GrantToken, user, session, agent, false); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedActionTypedSensitiveConsumers(t *testing.T) {
	for _, item := range []struct {
		action  string
		targets []string
	}{
		{"execution:create", []string{"synthetic-agent-a", "synthetic-agent-b"}},
		{"execution:cancel", []string{"synthetic-execution"}},
		{"rescue:create", []string{"synthetic-agent", "1", "0"}},
		{"rescue:cancel", []string{"synthetic-rescue-session"}},
		{"privileged:confirm", []string{"synthetic-agent", "7"}},
		{"enrollment:revoke", []string{"synthetic-agent"}},
	} {
		t.Run(item.action, func(t *testing.T) {
			ctx, _, _ := actionFixture(t)
			token := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource(item.action, item.targets...))
			if err := verifyConnectCredential(ctx, &commonv1.TwoFactorProof{AuthorizationToken: token}, item.action, item.targets...); err != nil {
				t.Fatal(err)
			}
		})
	}
}
