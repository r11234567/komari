package admin

import (
	"github.com/komari-monitor/komari/web/signonauth"
	"image/png"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/internal/credentialaction"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
	"github.com/pquerna/otp/totp"
)

func Generate2FA(c *gin.Context) {
	secret, img, err := accounts.Generate2Fa()
	if err != nil {
		api.RespondError(c, 500, "Failed to generate 2FA: "+err.Error())
		return
	}
	security.SetStrictSensitiveCookie(c, security.TwoFactorSecretCookieName, secret, 1800)
	c.Header("Content-Type", "image/png")
	c.Writer.WriteHeader(200)
	png.Encode(c.Writer, img)
}

func Enable2FA(c *gin.Context) {
	uuidRaw, _ := c.Get("uuid")
	userUUID, _ := uuidRaw.(string)
	secret, _ := c.Cookie(security.TwoFactorSecretCookieName)
	code := c.Query("code")
	if secret == "" || userUUID == "" || code == "" {
		api.RespondError(c, 400, "2FA secret or code not provided")
		return
	}
	user, err := accounts.GetUserByUUID(userUUID)
	if err != nil {
		api.RespondError(c, http.StatusUnauthorized, "Invalid user")
		return
	}
	if !totp.Validate(code, secret) {
		api.RespondError(c, 400, "Invalid 2FA code")
		return
	}
	hasFactor, err := credentialaction.HasSecondFactor(c.Request.Context(), userUUID)
	if err != nil {
		api.RespondError(c, http.StatusServiceUnavailable, "Failed to verify second factor state")
		return
	}
	if user.TwoFactor != "" || hasFactor {
		if err := api.VerifySensitive2FA(c); err != nil {
			api.RespondError(c, http.StatusUnauthorized, err.Error())
			return
		}
	}
	err = accounts.Enable2Fa(userUUID, secret)
	if err != nil {
		api.RespondError(c, 500, "Failed to enable 2FA: "+err.Error())
		return
	}
	security.ClearCookie(c, security.TwoFactorSecretCookieName, security.LegacyTwoFactorSecretCookieName)

	api.RespondSuccess(c, "2FA enabled successfully")
}

func Disable2FA(c *gin.Context) {
	uuid, _ := c.Get("uuid")
	err := accounts.Disable2Fa(uuid.(string))
	if err != nil {
		api.RespondError(c, 500, "Failed to disable 2FA: "+err.Error())
		return
	}
	api.RespondSuccess(c, "")
}

func Verify2FA(c *gin.Context) {
	api.RespondSuccess(c, "2FA verified successfully")
}

// UnlockSignOn is separate from account-page verification and workspace unlocks.
func UnlockSignOn(c *gin.Context) {
	session, _ := c.Cookie(security.SessionCookieName)
	var token string
	var err error
	if c.GetBool("trusted_credential_verified") {
		token, err = signonauth.CreateVerified(api.GetPrincipal(c), session, c.GetString("trusted_credential_state"))
	} else {
		token, err = signonauth.Create(api.GetPrincipal(c), session)
	}
	if err != nil {
		api.RespondError(c, http.StatusUnauthorized, err.Error())
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: signonauth.CookieName, Value: token,
		Path: "/", MaxAge: int(signonauth.Lifetime.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.SetCookie(c.Writer, &http.Cookie{Name: signonauth.LegacyCookieName,
		Path: "/api/admin/settings", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	c.Header("Cache-Control", "no-store")
	api.RespondSuccess(c, gin.H{"expires_in": int(signonauth.Lifetime.Seconds())})
}
