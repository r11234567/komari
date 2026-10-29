package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
)

// These operations require an interactive administrator, never an API key or
// plugin principal. Writes additionally consume a fresh resource-bound proof.
func credentialConsoleOnly(c *gin.Context) {
	principal := api.GetPrincipal(c)
	if principal == nil || principal.Type != rpc.PrincipalUser || principal.IsAPIKey || !principal.HasRole(rpc.RoleAdmin) {
		api.RespondError(c, http.StatusForbidden, "interactive administrator session required")
		c.Abort()
		return
	}
	c.Next()
}
func requireTrustedLifecycleWrite(c *gin.Context) bool {
	if !c.GetBool("trusted_credential_verified") {
		api.RespondError(c, http.StatusUnauthorized, "fresh trusted credential verification required")
		return false
	}
	return true
}
func RegisterCredentialLifecycleRoutes(group *gin.RouterGroup) {
	routes := group.Group("/agent-credentials", credentialConsoleOnly)
	routes.GET("", GetCredentialLifecycle)
	routes.POST("/policy", api.RequireSensitive2FA(), UpdateCredentialLifecyclePolicy)
	routes.POST("/:agent/reapprove", api.RequireSensitive2FA(), ReapproveAgentCredential)
}
func GetCredentialLifecycle(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 || page > 100000 {
		api.RespondError(c, 400, "invalid page")
		return
	}
	policy, err := enrollment.CredentialPolicy(c.Request.Context())
	if err != nil {
		api.RespondError(c, 500, "credential policy unavailable")
		return
	}
	rows, total, err := enrollment.CredentialStatuses(c.Request.Context(), page)
	if err != nil {
		api.RespondError(c, 500, "credential status unavailable")
		return
	}
	api.RespondSuccess(c, gin.H{"policy": policy, "credentials": rows, "total": total, "page": page, "page_size": 100})
}
func UpdateCredentialLifecyclePolicy(c *gin.Context) {
	if !requireTrustedLifecycleWrite(c) {
		return
	}
	var policy models.AgentCredentialPolicy
	if c.ShouldBindJSON(&policy) != nil {
		api.RespondError(c, 400, "invalid credential policy")
		return
	}
	saved, err := enrollment.SaveCredentialPolicy(c.Request.Context(), policy)
	if errors.Is(err, enrollment.ErrRefreshConflict) {
		api.RespondError(c, 409, "credential policy changed; reload and verify again")
		return
	}
	if err != nil {
		api.RespondError(c, 400, err.Error())
		return
	}
	auditlog.Log(c.ClientIP(), api.GetPrincipal(c).UserUUID, "updated Agent credential lifecycle policy", "warn")
	api.RespondSuccess(c, saved)
}
func ReapproveAgentCredential(c *gin.Context) {
	if !requireTrustedLifecycleWrite(c) {
		return
	}
	var input struct {
		Generation  uint64 `json:"generation"`
		Fingerprint string `json:"fingerprint"`
	}
	if c.ShouldBindJSON(&input) != nil || len(input.Fingerprint) != 43 {
		api.RespondError(c, 400, "current generation and key fingerprint required")
		return
	}
	actor := api.GetPrincipal(c).UserUUID
	err := enrollment.ReapproveCredential(c.Request.Context(), c.Param("agent"), input.Generation, input.Fingerprint, actor)
	if errors.Is(err, enrollment.ErrRefreshConflict) {
		api.RespondError(c, 409, "identity changed; reload and verify again")
		return
	}
	if err != nil {
		api.RespondError(c, 400, err.Error())
		return
	}
	auditlog.Log(c.ClientIP(), actor, "reauthorized Agent enrollment deadline: client="+c.Param("agent"), "warn")
	api.RespondSuccess(c, gin.H{"accepted": true})
}
