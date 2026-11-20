package security

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// SetSensitiveCookie applies the same transport and browser protections to
// short-lived OAuth, 2FA, and sharing cookies.
func SetSensitiveCookie(c *gin.Context, name, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

const SessionCookieName = "__Host-komari_session"

// Lax permits top-level OAuth GET callbacks to retain the signed-in session for account binding.
func SetSessionCookie(headers http.Header, value string, maxAge int) {
	cookie := http.Cookie{Name: SessionCookieName, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	headers.Add("Set-Cookie", cookie.String())
	legacy := http.Cookie{Name: "session_token", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	headers.Add("Set-Cookie", legacy.String())
}
