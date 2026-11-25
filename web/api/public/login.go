package public

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"

	"github.com/gin-gonic/gin"
)

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TwoFa    string `json:"2fa_code"`
}

const sessionCookieMaxAge = 86400 // 24 hours

func setSessionCookie(c *gin.Context, value string, maxAge int) {
	security.SetSessionCookie(c.Writer.Header(), value, maxAge)
}

func Login(c *gin.Context) {
	DisablePasswordLogin, _ := config.GetAs[bool](config.DisablePasswordLoginKey, false)
	if DisablePasswordLogin {
		api.RespondError(c, http.StatusForbidden, "Password login is disabled")
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}
	var data LoginRequest
	err = json.Unmarshal(bodyBytes, &data)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}
	if data.Username == "" || data.Password == "" {
		api.RespondError(c, http.StatusBadRequest, "Invalid request body: Username and password are required")
		return
	}

	uuid, success := accounts.CheckPassword(data.Username, data.Password)
	if !success {
		api.RespondError(c, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	// 2FA
	user, err := accounts.GetUserByUUID(uuid)
	if err != nil {
		api.RespondError(c, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	contacts, err := accounts.ListCredentialContacts(c.Request.Context(), uuid)
	passkey, passkeyErr := passkeycredential.Read(c.Request.Context(), uuid)
	if err != nil || passkeyErr != nil || len(contacts) != 0 || len(passkey.Records) != 0 {
		api.RespondError(c, http.StatusUnauthorized, "Use the two-step password login flow")
		return
	}
	if user.TwoFactor != "" { // 开启了2FA
		if data.TwoFa == "" {
			api.RespondError(c, http.StatusUnauthorized, "2FA code is required")
			return
		}
		if ok, err := accounts.Verify2Fa(uuid, data.TwoFa); err != nil || !ok {
			api.RespondError(c, http.StatusUnauthorized, "Invalid 2FA code")
			return
		}
	}
	// Create session
	session, err := accounts.CreateSession(uuid, sessionCookieMaxAge, c.Request.UserAgent(), c.ClientIP(), "password")
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "Failed to create session: "+err.Error())
		return
	}
	setSessionCookie(c, session, sessionCookieMaxAge)
	auditlog.Log(c.ClientIP(), uuid, "logged in (password)", "login")
	api.RespondSuccess(c, gin.H{"authenticated": true})
}
func Logout(c *gin.Context) {
	session, _ := c.Cookie(security.SessionCookieName)
	accounts.DeleteSession(session)
	if legacy, err := c.Cookie(security.LegacySessionCookieName); err == nil && legacy != "" && legacy != session {
		accounts.DeleteSession(legacy)
	}
	setSessionCookie(c, "", -1)
	security.ClearCookie(c, security.BindTicketCookieName)
	auditlog.Log(c.ClientIP(), "", "logged out", "logout")
	c.Redirect(302, "/")
}
