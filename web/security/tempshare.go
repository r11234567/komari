package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/config"
)

const (
	TempShareConfigKey       = "temporary_status_share"
	LegacyTempShareTokenKey  = "tempory_share_token"
	LegacyTempShareExpiryKey = "tempory_share_token_expire_at"
	MaxTempShareHours        = 24 * 30
)

type TempShare struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

func CurrentTempShare() (TempShare, error) {
	share, err := config.GetAs[TempShare](TempShareConfigKey, TempShare{})
	if share.ExpiresAt <= time.Now().Unix() {
		return TempShare{}, err
	}
	return share, err
}

func CreateTempShare(hours int) (TempShare, error) {
	if hours < 1 || hours > MaxTempShareHours {
		return TempShare{}, errors.New("invalid temporary share duration")
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return TempShare{}, err
	}
	share := TempShare{
		Token:     base64.RawURLEncoding.EncodeToString(key[:]),
		ExpiresAt: time.Now().Add(time.Duration(hours) * time.Hour).Unix(),
	}
	return share, config.Set(TempShareConfigKey, share)
}

func RevokeTempShare() error {
	return config.Set(TempShareConfigKey, TempShare{})
}

func tempShareMatches(candidate, expected string) bool {
	return expected != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

func tempShareCookie(token string) string {
	digest := sha256.Sum256([]byte("komari temporary share cookie v1\x00" + token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// RedeemTempShare exchanges the link on a dedicated route before loading any theme.
func RedeemTempShare(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	if !IsRequestSecure(c.Request) {
		c.Status(http.StatusForbidden)
		return
	}
	share, err := CurrentTempShare()
	if err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	remaining := share.ExpiresAt - time.Now().Unix()
	if remaining > 0 && tempShareMatches(c.Query("temp_key"), share.Token) {
		SetSensitiveCookie(c, TempShareCookieName, tempShareCookie(share.Token), int(remaining))
	}
	c.Redirect(http.StatusSeeOther, "/")
}

// HasTempAccess recognizes the current share on the public read surface only.
func HasTempAccess(c *gin.Context) bool {
	if c == nil || !IsRequestSecure(c.Request) {
		return false
	}
	value, err := c.Cookie(TempShareCookieName)
	if err != nil {
		return false
	}
	share, err := CurrentTempShare()
	return err == nil && share.Token != "" && tempShareMatches(value, tempShareCookie(share.Token))
}
