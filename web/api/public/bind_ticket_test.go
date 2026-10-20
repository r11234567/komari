package public

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/pkg/bindticket"
	"github.com/stretchr/testify/assert"
)

func TestRequestBindTicket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user, err := accounts.CreateAccount("bindtester", "validpassword")
	assert.NoError(t, err)
	assert.NotEmpty(t, user.UUID)

	t.Run("successful bind ticket issuance", func(t *testing.T) {
		router := gin.New()
		router.POST("/api/bind/ticket", RequestBindTicket)

		body, _ := json.Marshal(BindTicketRequest{
			Username: "bindtester",
			Password: "validpassword",
		})
		req, _ := http.NewRequest(http.MethodPost, "/api/bind/ticket", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
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
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		assert.NoError(t, err)
		assert.Equal(t, "success", resp.Status)
		assert.NotEmpty(t, resp.Data.Ticket)
		assert.Equal(t, 120, resp.Data.ExpiresIn)

		// Crucial security requirement: MUST NOT set session_token cookie!
		for _, cookie := range w.Result().Cookies() {
			assert.NotEqual(t, "session_token", cookie.Name)
		}

		// Verify ticket signature and claims
		key := accounts.SessionDigestKey()
		claims, err := bindticket.Verify(resp.Data.Ticket, key)
		assert.NoError(t, err)
		assert.Equal(t, user.UUID, claims.Sub)
		assert.Equal(t, bindticket.TicketType, claims.Type)
	})

	t.Run("invalid credentials rejected", func(t *testing.T) {
		router := gin.New()
		router.POST("/api/bind/ticket", RequestBindTicket)

		body, _ := json.Marshal(BindTicketRequest{
			Username: "bindtester",
			Password: "wrongpassword",
		})
		req, _ := http.NewRequest(http.MethodPost, "/api/bind/ticket", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}
