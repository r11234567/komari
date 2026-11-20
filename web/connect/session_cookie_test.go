package connectapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/api/admin"
	"github.com/komari-monitor/komari/web/api/public"
	"github.com/komari-monitor/komari/web/rpc/jsonrpc"
	"github.com/komari-monitor/komari/web/security"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
)

func TestSessionCookieAttributesLogoutAndLegacyRejection(t *testing.T) {
	// 1. SetSessionCookie sets __Host-komari_session (Secure, HttpOnly, Path=/, empty Domain, SameSite=Lax)
	//    and explicitly expires legacy session_token.
	rec := httptest.NewRecorder()
	security.SetSessionCookie(rec.Header(), "raw-session-value", 3600)
	cookies := rec.Result().Cookies()
	var hostCookie, legacyCookie *http.Cookie
	for _, c := range cookies {
		switch c.Name {
		case security.SessionCookieName:
			hostCookie = c
		case security.LegacySessionCookieName:
			legacyCookie = c
		}
	}
	if hostCookie == nil || !hostCookie.Secure || !hostCookie.HttpOnly || hostCookie.Path != "/" || hostCookie.Domain != "" || hostCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected __Host-komari_session attributes: %+v", hostCookie)
	}
	if legacyCookie == nil || legacyCookie.MaxAge >= 0 || legacyCookie.Path != "/" {
		t.Fatalf("legacy session_token was not expired: %+v", legacyCookie)
	}

	// 2. Real session authenticated only via __Host-komari_session, NEVER via legacy session_token.
	user, err := accounts.CreateAccount("cookie-user-"+uuid.NewString(), "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	session, err := accounts.CreateSession(user.UUID, 3600, "ua", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db := dbcore.GetDBInstance()
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", user.UUID).Delete(&models.User{})
	})

	r := gin.New()
	r.GET("/whoami", func(c *gin.Context) {
		p := api.IdentifyPrincipal(c)
		c.JSON(http.StatusOK, gin.H{"type": int(p.Type), "user": p.UserUUID})
	})
	r.POST("/api/logout", public.Logout)
	r.POST("/api/admin/oauth/bind", admin.BindingExternalAccount)
	r.POST("/api/rpc2", jsonrpc.OnRpcRequest)

	// Legacy cookie alone must be anonymous.
	legacyReq := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	legacyReq.AddCookie(&http.Cookie{Name: security.LegacySessionCookieName, Value: session})
	legacyRec := httptest.NewRecorder()
	r.ServeHTTP(legacyRec, legacyReq)
	var legacyResp struct {
		Type rpc.PrincipalType `json:"type"`
		User string            `json:"user"`
	}
	_ = json.Unmarshal(legacyRec.Body.Bytes(), &legacyResp)
	if legacyResp.Type != rpc.PrincipalAnonymous {
		t.Fatalf("legacy session_token cookie was accepted: %+v", legacyResp)
	}

	// __Host-komari_session authenticates user.
	hostReq := httptest.NewRequest(http.MethodGet, "/whoami", nil)
	hostReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	hostRec := httptest.NewRecorder()
	r.ServeHTTP(hostRec, hostReq)
	var hostResp struct {
		Type rpc.PrincipalType `json:"type"`
		User string            `json:"user"`
	}
	_ = json.Unmarshal(hostRec.Body.Bytes(), &hostResp)
	if hostResp.Type != rpc.PrincipalUser || hostResp.User != user.UUID {
		t.Fatalf("__Host-komari_session did not authenticate user: %+v", hostResp)
	}

	// OAuth bind rejects legacy/invalid session with 401 (not 500) and succeeds with __Host-komari_session.
	badBindReq := httptest.NewRequest(http.MethodPost, "/api/admin/oauth/bind", nil)
	badBindReq.AddCookie(&http.Cookie{Name: security.LegacySessionCookieName, Value: session})
	badBindRec := httptest.NewRecorder()
	r.ServeHTTP(badBindRec, badBindReq)
	if badBindRec.Code != http.StatusUnauthorized {
		t.Fatalf("OAuth bind with legacy cookie status = %d, want 401", badBindRec.Code)
	}

	goodBindReq := httptest.NewRequest(http.MethodPost, "/api/admin/oauth/bind", nil)
	goodBindReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	goodBindRec := httptest.NewRecorder()
	r.ServeHTTP(goodBindRec, goodBindReq)
	if goodBindRec.Code != http.StatusOK {
		t.Fatalf("OAuth bind with host cookie status = %d, body=%s", goodBindRec.Code, goodBindRec.Body.String())
	}

	// RPC2 public:getMe rejects legacy session_token cookie and accepts __Host-komari_session.
	getMeBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "public:getMe",
		"params":  map[string]any{},
	})
	legacyRPCReq := httptest.NewRequest(http.MethodPost, "/api/rpc2", bytes.NewReader(getMeBody))
	legacyRPCReq.Header.Set("Content-Type", "application/json")
	legacyRPCReq.AddCookie(&http.Cookie{Name: security.LegacySessionCookieName, Value: session})
	legacyRPCRec := httptest.NewRecorder()
	r.ServeHTTP(legacyRPCRec, legacyRPCReq)
	var legacyRPCResp struct {
		Result struct {
			LoggedIn bool `json:"logged_in"`
		} `json:"result"`
	}
	_ = json.Unmarshal(legacyRPCRec.Body.Bytes(), &legacyRPCResp)
	if legacyRPCResp.Result.LoggedIn {
		t.Fatalf("RPC2 common:getMe accepted legacy session_token cookie: %s", legacyRPCRec.Body.String())
	}

	hostRPCReq := httptest.NewRequest(http.MethodPost, "/api/rpc2", bytes.NewReader(getMeBody))
	hostRPCReq.Header.Set("Content-Type", "application/json")
	hostRPCReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	hostRPCRec := httptest.NewRecorder()
	r.ServeHTTP(hostRPCRec, hostRPCReq)
	var hostRPCResp struct {
		Result struct {
			LoggedIn bool   `json:"logged_in"`
			UUID     string `json:"uuid"`
		} `json:"result"`
	}
	_ = json.Unmarshal(hostRPCRec.Body.Bytes(), &hostRPCResp)
	if !hostRPCResp.Result.LoggedIn || hostRPCResp.Result.UUID != user.UUID {
		t.Fatalf("RPC2 common:getMe rejected __Host-komari_session: %s", hostRPCRec.Body.String())
	}

	// Connect maintenanceService: ListSessions returns stored v1: CurrentSession,
	// DeleteSession rejects raw browser cookie and accepts stored v1: session ID.
	adminCtx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{
		Principal:    rpc.NewUserPrincipal(user.UUID),
		Permission:   rpc.RoleAdmin,
		UserUUID:     user.UUID,
		SessionToken: session,
		RemoteIP:     "127.0.0.1",
	})
	maint := &maintenanceService{}
	listResp, err := maint.ListSessions(adminCtx, connect.NewRequest(&adminv1.ListSessionsRequest{}))
	if err != nil || listResp.Msg.CurrentSession != accounts.SessionIDForCookie(session) {
		t.Fatalf("ListSessions unexpected result: resp=%+v err=%v", listResp, err)
	}
	if _, err := maint.DeleteSession(adminCtx, connect.NewRequest(&adminv1.DeleteSessionRequest{Session: session})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Connect DeleteSession accepted raw cookie or wrong code: %v", err)
	}
	secondSession, err := accounts.CreateSession(user.UUID, 3600, "ua-2", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maint.DeleteSession(adminCtx, connect.NewRequest(&adminv1.DeleteSessionRequest{Session: accounts.SessionIDForCookie(secondSession)})); err != nil {
		t.Fatalf("Connect DeleteSession rejected valid stored v1: ID: %v", err)
	}
	if _, err := accounts.GetSession(secondSession); err == nil {
		t.Fatal("second session still active after Connect DeleteSession by stored ID")
	}

	// Logout clears both cookies and revokes session from DB.
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	logoutReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	logoutRec := httptest.NewRecorder()
	r.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusFound {
		t.Fatalf("logout status = %d, want 302", logoutRec.Code)
	}
	clearedHost, clearedLegacy := false, false
	for _, sc := range logoutRec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(sc, security.SessionCookieName+"=") && strings.Contains(sc, "Max-Age=0") {
			clearedHost = true
		}
		if strings.HasPrefix(sc, security.LegacySessionCookieName+"=") && strings.Contains(sc, "Max-Age=0") {
			clearedLegacy = true
		}
	}
	if !clearedHost || !clearedLegacy {
		t.Fatalf("logout did not clear both cookies: %v", logoutRec.Header().Values("Set-Cookie"))
	}
	if _, err := accounts.GetSession(session); err == nil {
		t.Fatal("session remained active in DB after logout")
	}
}
