package security

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	SessionCookieName               = "__Host-komari_session"
	LegacySessionCookieName         = "session_token"
	OAuthStateCookieName            = "__Host-komari_oauth_state"
	LegacyOAuthStateCookieName      = "oauth_state"
	OAuthReturnToCookieName         = "__Host-komari_oauth_return_to"
	LegacyOAuthReturnToCookieName   = "oauth_return_to"
	OAuthBindCookieName             = "__Host-komari_oauth_bind"
	LegacyOAuthBindCookieName       = "binding_external_account"
	TwoFactorSecretCookieName       = "__Host-komari_2fa_secret"
	LegacyTwoFactorSecretCookieName = "2fa_secret"
	TempShareCookieName             = "__Host-komari_temp_key"
	LegacyTempShareCookieName       = "temp_key"
)

func setCookie(c *gin.Context, name, value string, maxAge int, sameSite http.SameSite) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: sameSite,
	})
}

// SetSensitiveCookie applies the same transport and browser protections to
// short-lived OAuth and sharing cookies that must survive top-level GET redirects.
func SetSensitiveCookie(c *gin.Context, name, value string, maxAge int) {
	setCookie(c, name, value, maxAge, http.SameSiteLaxMode)
}

// SetStrictSensitiveCookie issues a host-scoped SameSite=Strict cookie for
// same-origin control-plane ceremonies such as TOTP enrollment.
func SetStrictSensitiveCookie(c *gin.Context, name, value string, maxAge int) {
	setCookie(c, name, value, maxAge, http.SameSiteStrictMode)
}

// ClearCookie expires one or more root-scoped cookies.
func ClearCookie(c *gin.Context, names ...string) {
	for _, name := range names {
		setCookie(c, name, "", -1, http.SameSiteLaxMode)
	}
}

// Lax permits top-level OAuth GET callbacks to retain the signed-in session for account binding.
func SetSessionCookie(headers http.Header, value string, maxAge int) {
	cookie := http.Cookie{Name: SessionCookieName, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	headers.Add("Set-Cookie", cookie.String())
	legacy := http.Cookie{Name: LegacySessionCookieName, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	headers.Add("Set-Cookie", legacy.String())
}
