package admin

import (
	"errors"
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/history"
)

func StartHistoryExport(c *gin.Context) {
	var request history.QueryRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "INVALID_REQUEST", "message": err.Error()})
		return
	}
	job, err := history.StartExport(request, filepath.Join("data", "exports"))
	if err != nil {
		status, code := http.StatusBadRequest, "INVALID_REQUEST"
		if errors.Is(err, history.ErrExportQueueFull) {
			status, code = http.StatusServiceUnavailable, "EXPORT_QUEUE_FULL"
		}
		c.JSON(status, gin.H{"status": "error", "code": code, "message": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "success", "data": job})
}

func GetHistoryExport(c *gin.Context) {
	job := history.GetExport(c.Param("id"))
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "code": "EXPORT_NOT_FOUND"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "data": job})
}

func DownloadHistoryExport(c *gin.Context) {
	path, ok := history.ExportFile(c.Param("id"))
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "code": "EXPORT_NOT_READY"})
		return
	}
	c.FileAttachment(path, filepath.Base(path))
}

func CancelHistoryExport(c *gin.Context) {
	if !history.CancelExport(c.Param("id")) {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "code": "EXPORT_NOT_FOUND"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}
