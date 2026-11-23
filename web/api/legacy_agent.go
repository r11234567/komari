package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/security"
	"gorm.io/gorm"
)

// RequireLegacyAgent keeps legacy routes available for unenrolled devices.
// Enrolled devices have signed Connect equivalents for these operations.
func RequireLegacyAgent(c *gin.Context) {
	principal := GetPrincipal(c)
	if principal == nil || principal.Type != rpc.PrincipalAgent {
		c.Next()
		return
	}
	_, err := enrollment.ResolveAgentPublicKey(principal.ClientUUID)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.Next()
	case err != nil:
		security.AbortRequest(c, http.StatusServiceUnavailable, "cannot determine agent credential policy")
	default:
		security.AbortRequest(c, http.StatusForbidden, "enrolled agents must use the signed Connect procedure")
	}
}
