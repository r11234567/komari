package public

import (
	"math"
	"net/http"
	"time"

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
	uuid, sessionCreatedAt, err := accounts.GetSessionIdentity(session)
	if err != nil || uuid == "" {
		api.RespondError(c, http.StatusUnauthorized, "Console session is invalid or expired")
		return
	}
	sessionAge := time.Since(sessionCreatedAt)
	remainingFreshness := bindticket.TicketDuration - sessionAge
	if remainingFreshness <= 0 {
		c.Header("Bind-Ticket-Reason", "fresh_login_required")
		api.RespondError(c, http.StatusUnauthorized, "A fresh console login is required to authorize binding")
		return
	}
	if remainingFreshness > bindticket.TicketDuration {
		remainingFreshness = bindticket.TicketDuration
	}
	// Keep expiry whole-second aligned with the response and never extend past
	// the original two-minute freshness window.
	ticketDuration := time.Duration(math.Ceil(remainingFreshness.Seconds())) * time.Second
	if ticketDuration <= 0 {
		ticketDuration = time.Second
	}

	key := accounts.SessionDigestKey()
	ticket, exp, err := bindticket.IssueWithDuration(uuid, key, ticketDuration)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "Failed to issue bind ticket: "+err.Error())
		return
	}

	auditlog.Log(c.ClientIP(), uuid, "issued reusable 2-minute bind ticket", "bind_ticket")

	c.Header("Cache-Control", "no-store")
	api.RespondSuccess(c, gin.H{
		"ticket":     ticket,
		"expires_in": int(ticketDuration.Seconds()),
		"expires_at": exp,
	})
}
