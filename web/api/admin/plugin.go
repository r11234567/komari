package admin

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/web/api"
)

// UploadPlugin installs a plugin ZIP package. The request body is the raw
// ZIP, mirroring UploadTheme.
func UploadPlugin(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, pluginMarketPackageMaxSize)

	tempFile, err := os.CreateTemp("", "komari-plugin-*.zip")
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "保存文件失败: "+err.Error())
		return
	}
	tempPath := tempFile.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if written, err := io.Copy(tempFile, c.Request.Body); err != nil || written == 0 {
		_ = tempFile.Close()
		api.RespondError(c, http.StatusBadRequest, "invalid or oversized plugin archive")
		return
	}
	if err := tempFile.Close(); err != nil {
		api.RespondError(c, http.StatusInternalServerError, "保存文件失败: "+err.Error())
		return
	}

	info, err := plugin.InstallZip(tempPath)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	api.RespondSuccessMessage(c, "插件上传成功", info)
}

// ServePluginFile serves a static file from an installed plugin directory,
// used by injected plugin admin pages.
func ServePluginFile(c *gin.Context) {
	name := strings.TrimPrefix(c.Param("filepath"), "/")
	file, err := plugin.OpenFile(c.Param("short"), name)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	http.ServeContent(c.Writer, c.Request, filepath.Base(name), info.ModTime(), file)
}
