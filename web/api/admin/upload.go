package admin

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	logger "github.com/komari-monitor/komari/utils/log"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/backup"
)

// UploadBackup receives a backup and schedules graceful shutdown to apply it on startup.
func UploadBackup(c *gin.Context) {
	restoreLock, status, err := backup.StageHTTPUpload(c.Writer, c.Request)
	if err != nil {
		api.RespondError(c, status, err.Error())
		return
	}
	backup.MarkRestartPending()

	c.JSON(http.StatusOK, gin.H{
		"status":              "success",
		"message":             "Backup uploaded successfully. The server is shutting down cleanly to apply the backup on startup (restart the process if not supervised).",
		"path":                "./data/backup.zip",
		"restart_required":    true,
		"manual_restart_note": "If running outside a container or process supervisor, restart Komari manually after shutdown completes.",
	})
	go func() {
		// Keep restoreLock held through shutdown so no concurrent upload can race.
		_ = restoreLock
		logger.InfoArgs("admin-api", "Backup uploaded; requesting graceful shutdown to apply backup on startup...")
		time.Sleep(300 * time.Millisecond)
		backup.RequestGracefulRestart("admin_backup_upload")
	}()
}
