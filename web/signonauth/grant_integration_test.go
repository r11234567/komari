package signonauth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/api/admin"
	jsonrpc "github.com/komari-monitor/komari/web/rpc/jsonrpc"
	"github.com/komari-monitor/komari/web/signonauth"
	"github.com/pquerna/otp/totp"
)

func TestFresh2FAUnlocksOnlySameLoginSettingsSession(t *testing.T) {
	db := dbcore.GetDBInstance()
	user, session := uuid.NewString(), "synthetic-login-"+uuid.NewString()
	const secret = "JBSWY3DPEHPK3PXP"
	if err := db.Create(&models.User{UUID: user, Username: user, Passwd: "synthetic-test", TwoFactor: secret}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Session{UUID: user, Session: session, Expires: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Where("uuid = ?", user).Delete(&models.Session{})
		db.Where("uuid = ?", user).Delete(&models.User{})
	})
	r := gin.New()
	r.Use(func(c *gin.Context) { api.SetPrincipal(c, rpc.NewUserPrincipal(user)); c.Set("uuid", user); c.Next() })
	r.POST("/unlock", api.RequireSensitive2FA(), admin.UnlockSignOn)
	r.GET("/settings", func(c *gin.Context) {
		response := jsonrpc.CallFromGin(c, "admin:getOidcProvider", map[string]any{})
		if response.Error != nil {
			c.Status(http.StatusForbidden)
			return
		}
		c.Status(http.StatusOK)
	})
	call := func(path, code, login string, grant *http.Cookie) *httptest.ResponseRecorder {
		method := http.MethodGet
		if path == "/unlock" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(&http.Cookie{Name: "session_token", Value: login})
		if grant != nil {
			req.AddCookie(grant)
		}
		req.Header.Set("X-2FA-Code", code)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if rec := call("/unlock", "", session, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing code: %d", rec.Code)
	}
	if rec := call("/settings", "", session, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("no grant: %d", rec.Code)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rec := call("/unlock", code, session, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock: %d %s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, item := range rec.Result().Cookies() {
		if item.Name == signonauth.CookieName {
			cookie = item
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/api/admin/settings" || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe grant cookie: %v", cookie)
	}
	if rec := call("/settings", "", session, cookie); rec.Code != http.StatusOK {
		t.Fatalf("unlocked: %d", rec.Code)
	}
	if rec := call("/settings", "", "different-session", cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("cross session: %d", rec.Code)
	}
	if rec := call("/unlock", code, session, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed code: %d", rec.Code)
	}
	if err := db.Where("uuid = ?", user).Delete(&models.Session{}).Error; err != nil {
		t.Fatal(err)
	}
	if rec := call("/settings", "", session, cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked: %d", rec.Code)
	}
}
