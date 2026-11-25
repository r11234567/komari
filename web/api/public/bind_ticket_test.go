package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/bindticket"
	"github.com/komari-monitor/komari/web/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestBindTicket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, err := accounts.CreateAccount("bindtester", "validpassword")
	assert.NoError(t, err)
	assert.NotEmpty(t, user.UUID)

	t.Run("successful bind ticket issuance", func(t *testing.T) {
		router := gin.New()
		router.POST("/api/bind/ticket", RequestBindTicket)

		session, err := accounts.CreateSession(user.UUID, 3600, "test", "127.0.0.1", "password")
		assert.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "https://panel.example/api/bind/ticket", nil)
		req.Header.Set("Origin", "https://panel.example")
		req.AddCookie(&http.Cookie{Name: "__Host-komari_session", Value: session})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)

		var resp struct {
			Status string `json:"status"`
			Data   struct {
				Ticket    string `json:"ticket"`
				ExpiresIn int    `json:"expires_in"`
				ExpiresAt int64  `json:"expires_at"`
			} `json:"data"`
		}
		err = json.Unmarshal(w.Body.Bytes(), &resp)
		assert.NoError(t, err)
		assert.Equal(t, "success", resp.Status)
		assert.Empty(t, resp.Data.Ticket)
		assert.Equal(t, 120, resp.Data.ExpiresIn)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

		// Exchanging a session for a bind ticket must not rotate that session.
		cookies := w.Result().Cookies()
		require.Len(t, cookies, 1)
		assert.Equal(t, security.BindTicketCookieName, cookies[0].Name)
		assert.True(t, cookies[0].Secure)
		assert.True(t, cookies[0].HttpOnly)
		assert.Equal(t, http.SameSiteStrictMode, cookies[0].SameSite)

		// Verify ticket signature and claims
		key := bindticket.SessionKey(accounts.SessionDigestKey(), session)
		claims, err := bindticket.Verify(cookies[0].Value, key)
		assert.NoError(t, err)
		assert.Equal(t, user.UUID, claims.Sub)
		assert.Equal(t, bindticket.TicketType, claims.Type)
		assert.Equal(t, bindticket.TicketAudience, claims.Aud)
		assert.Equal(t, bindticket.TicketScope, claims.Scope)
	})

	t.Run("missing session rejected", func(t *testing.T) {
		router := gin.New()
		router.POST("/api/bind/ticket", RequestBindTicket)

		req := httptest.NewRequest(http.MethodPost, "https://panel.example/api/bind/ticket", nil)
		req.Header.Set("Origin", "https://panel.example")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("expired session rejected", func(t *testing.T) {
		router := gin.New()
		router.POST("/api/bind/ticket", RequestBindTicket)

		session, err := accounts.CreateSession(user.UUID, -1, "test", "127.0.0.1", "password")
		assert.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "https://panel.example/api/bind/ticket", nil)
		req.Header.Set("Origin", "https://panel.example")
		req.AddCookie(&http.Cookie{Name: "__Host-komari_session", Value: session})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("stale but otherwise valid session cannot mint another ticket", func(t *testing.T) {
		router := gin.New()
		router.POST("/api/bind/ticket", RequestBindTicket)

		session, err := accounts.CreateSession(user.UUID, 3600, "test", "127.0.0.1", "password")
		assert.NoError(t, err)
		err = dbcore.GetDBInstance().
			Model(&models.Session{}).
			Where("uuid = ?", user.UUID).
			Update("created_at", time.Now().UTC().Add(-bindticket.TicketDuration-time.Second)).Error
		assert.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "https://panel.example/api/bind/ticket", nil)
		req.Header.Set("Origin", "https://panel.example")
		req.AddCookie(&http.Cookie{Name: "__Host-komari_session", Value: session})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Equal(t, "fresh_login_required", w.Header().Get("Bind-Ticket-Reason"))
	})
}
