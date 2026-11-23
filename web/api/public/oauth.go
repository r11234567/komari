package public

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/utils"
	"github.com/komari-monitor/komari/web/oauth"
	"github.com/komari-monitor/komari/web/security"
)

func safeOAuthReturnTo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "/admin"
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return "/admin"
	}
	if parsed.Path == "/bind-login" || parsed.Path == "/login" ||
		parsed.Path == "/admin" || strings.HasPrefix(parsed.Path, "/admin/") {
		return value
	}
	return "/admin"
}

// /api/oauth
func OAuth(c *gin.Context) {
	OAuthEnabled, _ := config.GetAs[bool](config.OAuthEnabledKey, false)
	if !OAuthEnabled {
		c.JSON(403, gin.H{"status": "error", "error": "OAuth is not enabled"})
		return
	}

	callbackURL, err := utils.GetCallbackURL(c)
	if err != nil {
		c.JSON(400, gin.H{"status": "error", "error": "Failed to resolve OAuth callback URL: " + err.Error()})
		return
	}
	authURL, state := oauth.CurrentProvider().GetAuthorizationURL(callbackURL)
	if strings.TrimSpace(authURL) == "" {
		c.JSON(500, gin.H{"status": "error", "error": "OAuth provider returned empty authorization URL"})
		return
	}

	security.SetSensitiveCookie(c, security.OAuthStateCookieName, state, 3600)
	security.SetSensitiveCookie(c, security.OAuthReturnToCookieName, safeOAuthReturnTo(c.Query("returnTo")), 3600)

	c.Redirect(302, authURL)
}

// /api/oauth_callback
func OAuthCallback(c *gin.Context) {
	state, _ := c.Cookie(security.OAuthStateCookieName)
	returnTo := safeOAuthReturnTo("")
	if destination, err := c.Cookie(security.OAuthReturnToCookieName); err == nil {
		returnTo = safeOAuthReturnTo(destination)
	}
	bindNonce, _ := c.Cookie(security.OAuthBindCookieName)
	security.ClearCookie(
		c,
		security.OAuthStateCookieName,
		security.LegacyOAuthStateCookieName,
		security.OAuthReturnToCookieName,
		security.LegacyOAuthReturnToCookieName,
		security.OAuthBindCookieName,
		security.LegacyOAuthBindCookieName,
	)

	oauthEnabled, _ := config.GetAs[bool](config.OAuthEnabledKey, false)
	if !oauthEnabled {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "error": "OAuth is not enabled"})
		return
	}

	// 严格验证所有 OAuth 提供商的 state 匹配，防止跨站登录/账号绑定 CSRF
	queryState := c.Query("state")
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(queryState)) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Invalid state"})
		return
	}

	queries := make(map[string]string)
	for key, values := range c.Request.URL.Query() {
		if len(values) > 0 {
			queries[key] = values[0]
		}
	}
	callbackURL, err := utils.GetCallbackURL(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Failed to resolve OAuth callback URL: " + err.Error()})
		return
	}
	oidcUser, err := oauth.CurrentProvider().OnCallback(c, state, queries, callbackURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "Failed to get user info: " + err.Error()})
		return
	}

	// ID作为SSO ID
	sso_id := fmt.Sprintf("%s_%s", oauth.CurrentProvider().GetName(), oidcUser.UserId)

	// 若持有服务端登记且绑定当前会话的 bindNonce，则执行外部账号绑定；否则执行登录
	if bindNonce != "" {
		session, _ := c.Cookie(security.SessionCookieName)
		user, err := accounts.GetUserBySession(session)
		if err != nil || !oauth.ConsumeBindingToken(bindNonce, user.UUID, session) {
			c.JSON(http.StatusForbidden, gin.H{"status": "error", "message": "Binding failed"})
			return
		}
		err = accounts.BindingExternalAccount(user.UUID, sso_id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "message": "Binding failed"})
			return
		}
		auditlog.Log(c.ClientIP(), user.UUID, "bound external account (OAuth)"+fmt.Sprintf(",sso_id: %s", sso_id), "login")
		c.Redirect(302, returnTo)
		return
	}

	// 尝试获取用户
	user, err := accounts.GetUserBySSO(sso_id)
	if err != nil {
		c.JSON(401, gin.H{
			"status":  "error",
			"message": "please log in and bind your external account first.",
		})
		return
	}

	// 创建会话
	session, err := accounts.CreateSession(user.UUID, sessionCookieMaxAge, c.Request.UserAgent(), c.ClientIP(), "oauth")
	if err != nil {
		c.JSON(500, gin.H{"status": "error", "message": err.Error()})
		return
	}

	// 设置cookie并返回
	setSessionCookie(c, session, sessionCookieMaxAge)
	auditlog.Log(c.ClientIP(), user.UUID, "logged in (OAuth)", "login")
	c.Redirect(302, returnTo)
}
