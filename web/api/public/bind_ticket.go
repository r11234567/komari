package public

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/pkg/bindticket"
	"github.com/komari-monitor/komari/web/api"
)

// RequestBindTicket exchanges an existing console session for a reusable
// two-minute ticket scoped to enrollment authorization.
func RequestBindTicket(c *gin.Context) {
	session, err := c.Cookie("session_token")
	if err != nil || session == "" {
		api.RespondError(c, http.StatusUnauthorized, "A completed console login is required")
		return
	}
	uuid, err := accounts.GetSession(session)
	if err != nil || uuid == "" {
		api.RespondError(c, http.StatusUnauthorized, "Console session is invalid or expired")
		return
	}

	key := accounts.SessionDigestKey()
	ticket, exp, err := bindticket.Issue(uuid, key)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "Failed to issue bind ticket: "+err.Error())
		return
	}

	auditlog.Log(c.ClientIP(), uuid, "issued reusable 2-minute bind ticket", "bind_ticket")

	c.Header("Cache-Control", "no-store")
	api.RespondSuccess(c, gin.H{
		"ticket":     ticket,
		"expires_in": int(bindticket.TicketDuration.Seconds()),
		"expires_at": exp,
	})
}
