package api

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
)

// principalContextKey 是 principal 在 gin.Context 中的存储键。
const principalContextKey = "principal"

// IdentifyPrincipal reuses verified API credentials and resolves browser and device identities.
func IdentifyPrincipal(c *gin.Context) *rpc.Principal {
	if p := GetPrincipal(c); p != nil {
		return p
	}
	if token, bearer := requestBearer(c.GetHeader("Authorization")); bearer {
		return agentPrincipal(token)
	}
	if session, err := c.Cookie("session_token"); err == nil && session != "" {
		if uuid, err := accounts.GetSession(session); err == nil && uuid != "" {
			return rpc.NewUserPrincipal(uuid)
		}
	}
	return agentPrincipal(extractClientToken(c))
}

func requestBearer(header string) (string, bool) {
	scheme, value, _ := strings.Cut(strings.TrimSpace(header), " ")
	return value, strings.EqualFold(scheme, "Bearer")
}

func agentPrincipal(token string) *rpc.Principal {
	// 3. Client Token(agent 客户端)
	if token != "" {
		// 3a. Single-use install token — redeemed on first presentation, giving
		// the installer a one-shot RoleClient identity so the install script can
		// connect the agent without the permanent token ever leaving the panel.
		if enrollment.IsInstallToken(token) {
			if uuid, err := enrollment.RedeemInstallToken(token); err == nil && uuid != "" {
				return rpc.NewAgentPrincipal(uuid)
			}
			// A bad or already-used install token falls through to anonymous;
			// it must never accidentally match a permanent token.
			return rpc.NewAnonymousPrincipal()
		}
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
