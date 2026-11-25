package connectapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
)

// GuardAgentStream tracks the separately routed file data stream after its
// Agent access credential has been authenticated by the entry boundary.
func GuardAgentStream(c *gin.Context) {
	principal := api.GetPrincipal(c)
	if principal == nil || principal.Type != rpc.PrincipalAgent {
		security.AbortRequest(c, http.StatusUnauthorized, "an Agent credential is required")
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
