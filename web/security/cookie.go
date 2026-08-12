package security

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// SetSensitiveCookie applies the same transport and browser protections to
// short-lived OAuth, 2FA, sharing, and session cookies.
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
