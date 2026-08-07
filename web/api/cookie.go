package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/utils"
)

// SetCookie applies the same transport and browser protections to all
// authentication-related cookies while retaining plain-HTTP local access.
func SetCookie(c *gin.Context, name, value string, maxAge int, httpOnly bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   utils.GetScheme(c) == "https",
		HttpOnly: httpOnly,
		SameSite: http.SameSiteLaxMode,
	})
}
