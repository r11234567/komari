package public

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/pkg/bindticket"
	"github.com/komari-monitor/komari/web/api"
)

type BindTicketRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TwoFa    string `json:"2fa_code"`
}

// RequestBindTicket issues a 2-minute single-use ticket specifically for /bind machine authorization.
// Crucially, it does NOT alter, recreate, or invalidate regular console session tokens.
func RequestBindTicket(c *gin.Context) {
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}
	var data BindTicketRequest
	if err := json.Unmarshal(bodyBytes, &data); err != nil {
		api.RespondError(c, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}

	username := data.Username
	// If username was omitted but the user has an active session cookie, infer the username
	if username == "" {
		if session, err := c.Cookie("session_token"); err == nil && session != "" {
			if user, err := accounts.GetUserBySession(session); err == nil && user.Username != "" {
				username = user.Username
			}
		}
	}

	if username == "" || data.Password == "" {
		api.RespondError(c, http.StatusBadRequest, "Username and password are required")
		return
	}

	uuid, success := accounts.CheckPassword(username, data.Password)
	if !success {
		api.RespondError(c, http.StatusUnauthorized, "Invalid credentials")
		return
	}

	user, err := accounts.GetUserByUUID(uuid)
	if err == nil && user.TwoFactor != "" {
		if data.TwoFa == "" {
			api.RespondError(c, http.StatusUnauthorized, "2FA code is required")
			return
		}
		if ok, err := accounts.Verify2Fa(uuid, data.TwoFa); err != nil || !ok {
			api.RespondError(c, http.StatusUnauthorized, "Invalid 2FA code")
			return
		}
	}

	key := accounts.SessionDigestKey()
	ticket, exp, err := bindticket.Issue(uuid, key)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "Failed to issue bind ticket: "+err.Error())
		return
	}

	auditlog.Log(c.ClientIP(), uuid, "issued 2-minute bind ticket", "bind_ticket")

	api.RespondSuccess(c, gin.H{
		"ticket":     ticket,
		"expires_in": int(bindticket.TicketDuration.Seconds()),
		"expires_at": exp,
	})
}
