package security

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/config"
)

const tempShareRedeemedContextKey = "temp_share_redeemed"

func activeTempShareToken() (string, int64, bool) {
	expireAt, err := config.GetAs[int64]("tempory_share_token_expire_at", 0)
	if err != nil {
		return "", 0, false
	}
	allowKey, err := config.GetAs[string]("tempory_share_token", "")
	if err != nil || allowKey == "" {
		return "", 0, false
	}
	now := time.Now().Unix()
	if expireAt < now {
		return "", 0, false
	}
	return allowKey, expireAt - now, true
}

func tempShareMatches(candidate, allowKey string) bool {
	return len(candidate) > 0 && subtle.ConstantTimeCompare([]byte(candidate), []byte(allowKey)) == 1
}

// RedeemTempShareQuery exchanges a valid ?temp_key= query parameter on GET
// requests into the host-scoped temporary-share cookie and marks the request.
func RedeemTempShareQuery(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Method != http.MethodGet {
		return false
	}
	tempKey := strings.TrimSpace(c.Query("temp_key"))
	if tempKey == "" {
		return false
	}
	allowKey, remainingSeconds, ok := activeTempShareToken()
	if !ok || !tempShareMatches(tempKey, allowKey) {
		return false
	}
	if remainingSeconds > 0 {
		SetSensitiveCookie(c, TempShareCookieName, tempKey, int(remainingSeconds))
	}
	c.Set(tempShareRedeemedContextKey, true)
	return true
}

// HasTempAccess reports whether the request carries a valid temporary share
// token (from query redemption on the current request, the host-scoped cookie,
// or the client-visible legacy fallback cookie on plain-HTTP deployments).
func HasTempAccess(c *gin.Context) bool {
	if c == nil {
		return false
	}
	if c.GetBool(tempShareRedeemedContextKey) {
		return true
	}
	allowKey, _, ok := activeTempShareToken()
	if !ok {
		return false
	}
	for _, cookieName := range []string{TempShareCookieName, LegacyTempShareCookieName} {
		if value, err := c.Cookie(cookieName); err == nil && tempShareMatches(value, allowKey) {
			return true
		}
	}
	return false
}
