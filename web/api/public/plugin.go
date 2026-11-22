package public

import (
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/web/security"
)

// ServePluginFile serves a public iframe page declared in a plugin manifest
// (visibility=public). It is reachable without authentication; the plugin
// must be enabled and the requested file must be listed as a public page.
func ServePluginFile(c *gin.Context) {
	security.ServeSandboxedPluginPage(c, plugin.OpenPublicFile, false)
}
