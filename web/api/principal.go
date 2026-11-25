package api

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/security"
)

// principalContextKey 是 principal 在 gin.Context 中的存储键。
const principalContextKey = "principal"
const retiredInstallTokenPrefix = "ki_"

// IdentifyPrincipal reuses verified API credentials and resolves browser and device identities.
func IdentifyPrincipal(c *gin.Context) *rpc.Principal {
	if p := GetPrincipal(c); p != nil {
		return p
	}
	if scheme, token, ok := requestAuthCredential(c.GetHeader("Authorization")); ok {
		if scheme == "DPoP" && !strings.HasPrefix(c.FullPath(), "/komari.") {
			return rpc.NewAnonymousPrincipal()
		}
		return agentPrincipal(token)
	}
	if session, err := c.Cookie(security.SessionCookieName); err == nil && session != "" {
		if uuid, err := accounts.GetSession(session); err == nil && uuid != "" {
			return rpc.NewUserPrincipal(uuid)
		}
	}
	return agentPrincipal(extractClientToken(c))
}

func requestAuthCredential(header string) (scheme string, token string, ok bool) {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" {
		return "", "", false
	}
	idx := strings.IndexAny(trimmed, " \t")
	if idx < 0 {
		switch {
		case strings.EqualFold(trimmed, "Bearer"):
			return "Bearer", "", true
		case strings.EqualFold(trimmed, "DPoP"):
			return "DPoP", "", true
		default:
			return "", "", false
		}
	}
	switch {
	case strings.EqualFold(trimmed[:idx], "Bearer"):
		scheme = "Bearer"
	case strings.EqualFold(trimmed[:idx], "DPoP"):
		scheme = "DPoP"
	default:
		return "", "", false
	}
	return scheme, strings.TrimSpace(trimmed[idx+1:]), true
}

func requestBearer(header string) (string, bool) {
	scheme, token, ok := requestAuthCredential(header)
	return token, ok && scheme == "Bearer"
}

func agentPrincipal(token string) *rpc.Principal {
	if token != "" && !strings.HasPrefix(token, retiredInstallTokenPrefix) {
		if uuid, err := checkTokenAndGetUUID(token); err == nil && uuid != "" {
			return rpc.NewAgentPrincipal(uuid)
		}
	}

	// 4. 匿名访客
	return rpc.NewAnonymousPrincipal()
}

// SetPrincipal 将已识别的主体写入 gin.Context,供下游(RPC 边界等)复用。
func SetPrincipal(c *gin.Context, p *rpc.Principal) {
	c.Set(principalContextKey, p)
}

// GetPrincipal 读取已识别的主体;若中间件未运行则返回 nil。
func GetPrincipal(c *gin.Context) *rpc.Principal {
	if v, ok := c.Get(principalContextKey); ok {
		if p, ok := v.(*rpc.Principal); ok {
			return p
		}
	}
	return nil
}
