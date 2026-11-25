package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
)

func GetTemporaryShare(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	reveal := c.Query("reveal") == "true"
	if reveal && !verifyTemporaryShareAction(c) {
		return
	}
	share, err := security.CurrentTempShare()
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "Failed to read the temporary share")
		return
	}
	if !reveal {
		share.Token = ""
	}
	api.RespondSuccess(c, share)
}

func CreateTemporaryShare(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	var request struct {
		Hours int `json:"hours"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Hours < 1 || request.Hours > security.MaxTempShareHours {
		api.RespondError(c, http.StatusBadRequest, "Temporary share duration must be between 1 and 720 hours")
		return
	}
	if !verifyTemporaryShareAction(c) {
		return
	}
	share, err := security.CreateTempShare(request.Hours)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "Failed to create the temporary share")
		return
	}
	api.RespondSuccess(c, share)
}

func DeleteTemporaryShare(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !verifyTemporaryShareAction(c) {
		return
	}
	if err := security.RevokeTempShare(); err != nil {
		api.RespondError(c, http.StatusInternalServerError, "Failed to revoke the temporary share")
		return
	}
	api.RespondSuccess(c, security.TempShare{})
}

func verifyTemporaryShareAction(c *gin.Context) bool {
	if err := api.VerifySensitive2FA(c); err != nil {
		api.RespondError(c, http.StatusUnauthorized, err.Error())
		return false
	}
	return true
}
