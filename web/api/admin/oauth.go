package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/oauth"
	"github.com/komari-monitor/komari/web/security"
)

// oauth.go
// 外部账号绑定/解绑：绑定走 302 重定向，保留为 REST handler（不走 RPC 桥）。
// OIDC provider 配置的 get/set 已迁移为 RPC2 方法（见 web/rpc/jsonrpc/admin.provider.go）。

func BindingExternalAccount(c *gin.Context) {
	session, _ := c.Cookie(security.SessionCookieName)
	user, err := accounts.GetUserBySession(session)
	if err != nil {
		api.RespondError(c, http.StatusUnauthorized, "Unauthorized")
		return
	}
	nonce, err := oauth.IssueBindingToken(user.UUID, session)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "Failed to initiate OAuth binding")
		return
	}
	security.SetSensitiveCookie(c, security.OAuthBindCookieName, nonce, 300)
	api.RespondSuccess(c, gin.H{"redirect_url": "/api/oauth"})
}

func UnbindExternalAccount(c *gin.Context) {
	session, _ := c.Cookie(security.SessionCookieName)
	user, err := accounts.GetUserBySession(session)
	if err != nil {
		api.RespondError(c, http.StatusUnauthorized, "Unauthorized")
		return
	}
	if err := accounts.UnbindExternalAccount(user.UUID); err != nil {
		api.RespondError(c, 500, "Failed to unbind external account: "+err.Error())
		return
	}
	api.RespondSuccess(c, nil)
}
