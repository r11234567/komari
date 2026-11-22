package security

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

const pluginPageSandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals; frame-ancestors 'self'"

// ServeSandboxedPluginPage serves a manifest-declared plugin page asset inside an
// isolated iframe sandbox so same-origin credentials and APIs remain unreachable.
func ServeSandboxedPluginPage(c *gin.Context, openFn func(short, name string) (*os.File, error), noStore bool) {
	name := strings.TrimPrefix(c.Param("filepath"), "/")
	file, err := openFn(c.Param("short"), name)
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
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", pluginPageSandboxCSP)
	if noStore {
		c.Header("Cache-Control", "no-store")
	}
	http.ServeContent(c.Writer, c.Request, filepath.Base(name), info.ModTime(), file)
}
