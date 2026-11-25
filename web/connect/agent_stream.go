package connectapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/requestmeta"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
)

// GuardAgentTransfer verifies proof of possession before tracking the stream
// or handing the one-shot file transfer to its handler.
func GuardAgentTransfer(c *gin.Context) {
	principal := api.GetPrincipal(c)
	auth := strings.Fields(c.GetHeader("Authorization"))
	if principal == nil || principal.Type != rpc.PrincipalAgent || len(auth) != 2 || !strings.EqualFold(auth[0], "DPoP") {
		security.AbortRequest(c, http.StatusUnauthorized, "an Agent DPoP credential is required")
		return
	}
	if c.Request.URL.RawQuery != "" || c.Request.URL.ForceQuery {
		security.AbortRequest(c, http.StatusBadRequest, "file transfers do not accept query parameters")
		return
	}
	req, err := requestmeta.EnsureBound(c.Request)
	if err != nil {
		security.AbortRequest(c, http.StatusBadRequest, "invalid request origin")
		return
	}
	c.Request = req
	if err := verifyDPoP(c, principal); err != nil {
		security.SetPrincipalHeader(c, rpc.NewAnonymousPrincipal())
		security.AbortRequest(c, http.StatusUnauthorized, err.Error())
		return
	}
	serveAgentStream(c, principal, c.Next)
}

func serveAgentStream(c *gin.Context, principal *rpc.Principal, serve func()) {
	token := extractAuthSchemeToken(c.GetHeader("Authorization"))
	ctx, stop, err := enrollment.TrackStream(c.Request.Context(), principal.ClientUUID, token)
	if err != nil {
		security.AbortRequest(c, http.StatusUnauthorized, "Agent stream credential is no longer valid")
		return
	}
	defer stop()
	c.Request = c.Request.WithContext(ctx)
	controller := http.NewResponseController(c.Writer)
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		// A blocked HTTP read or write can outlive the derived context.
		// Interrupt both directions before returning control to net/http.
		now := time.Now()
		_ = controller.SetReadDeadline(now)
		_ = controller.SetWriteDeadline(now)
	})
	defer func() {
		if !stopInterrupt() {
			<-interrupted
		}
	}()
	serve()
}
