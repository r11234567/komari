package admin

import (
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	logger "github.com/komari-monitor/komari/utils/log"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/backup"
)

// UploadBackup receives a backup and applies it after the service restarts.
func UploadBackup(c *gin.Context) {
	restoreLock, status, err := backup.StageHTTPUpload(c.Writer, c.Request)
	if err != nil {
		api.RespondError(c, status, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "success",
		"message": "Backup uploaded successfully. The service will restart and apply the backup.",
		"path":    "./data/backup.zip",
	})
	go func() {
		logger.InfoArgs("admin-api", "Backup uploaded, restarting service in 2 seconds to apply on startup...")
		time.Sleep(2 * time.Second)
		restoreLock.Release()
		os.Exit(0)
	}()
}
