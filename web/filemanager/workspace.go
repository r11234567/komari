package filemanager

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/workspace"
)

func authorizeFileRequest(c *gin.Context) (context.CancelFunc, bool) {
	session, err := c.Cookie("session_token")
	if err != nil || session == "" {
		api.RespondError(c, http.StatusUnauthorized, workspace.ErrLocked.Error())
		return func() {}, false
	}
	principal := api.GetPrincipal(c)
	if principal == nil {
		principal = api.IdentifyPrincipal(c)
	}
	meta := &rpc.ContextMeta{Principal: principal, SessionToken: session, UserUUID: principal.UserUUID}
	token := c.GetHeader(workspace.Header)
	if token == "" {
		token, _ = c.Cookie(workspace.CookieName(c.Param("uuid")))
	}
	ctx := workspace.WithToken(rpc.NewContextWithMeta(c.Request.Context(), meta), token)
	bounded, cancel, _, err := workspace.Authorize(ctx, c.Param("uuid"))
	if err != nil {
		api.RespondError(c, http.StatusUnauthorized, err.Error())
		return cancel, false
	}
	c.Request = c.Request.WithContext(bounded)
	return cancel, true
}

func requestWorkspaceID(c *gin.Context) string {
	g := workspace.FromContext(c.Request.Context())
	if g == nil {
		return ""
	}
	return g.ID
}
