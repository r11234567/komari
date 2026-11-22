package admin

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/stretchr/testify/require"
)

func TestAdminServePluginFileEnforcesManifestSandboxAndEnableState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, plugin.CloseAll())
	oldDir, oldStorage := plugin.DataDir, plugin.StorageDir
	plugin.DataDir = t.TempDir()
	plugin.StorageDir = t.TempDir()
	t.Cleanup(func() {
		_ = plugin.CloseAll()
		plugin.DataDir = oldDir
		plugin.StorageDir = oldStorage
	})

	zipPath := filepath.Join(t.TempDir(), "adm.zip")
	f, err := os.Create(zipPath)
	require.NoError(t, err)
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{
		"komari-plugin.json": `{"name":"AdminPage","short":"adm-page","version":"1.0.0","pages":[{"file":"pages/admin.html","title":"Admin","visibility":"admin"}]}`,
		"script.js":          `function load(){}`,
		"pages/admin.html":   `<html><body>admin</body></html>`,
		"pages/admin.js":     `console.log("ok")`,
		"secret.env":         `SECRET=1`,
		".hidden":            `hidden`,
	} {
		entry, err := zw.Create(name)
		require.NoError(t, err)
		_, err = entry.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	plugin.Init(gin.New())
	_, err = plugin.InstallZip(zipPath)
	require.NoError(t, err)

	r := gin.New()
	r.GET("/api/admin/plugin/:short/*filepath", ServePluginFile)

	// Disabled plugin must not serve static iframe pages.
	recDisabled := httptest.NewRecorder()
	r.ServeHTTP(recDisabled, httptest.NewRequest(http.MethodGet, "/api/admin/plugin/adm-page/pages/admin.html", nil))
	require.Equal(t, http.StatusNotFound, recDisabled.Code)

	require.NoError(t, plugin.SetEnabled("adm-page", true, true))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/plugin/adm-page/pages/admin.html", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	csp := rec.Header().Get("Content-Security-Policy")
	require.Contains(t, csp, "sandbox")
	require.Contains(t, csp, "frame-ancestors 'self'")
	require.NotContains(t, csp, "allow-same-origin")

	// Sibling asset in the declared page directory is allowed.
	recAsset := httptest.NewRecorder()
	r.ServeHTTP(recAsset, httptest.NewRequest(http.MethodGet, "/api/admin/plugin/adm-page/pages/admin.js", nil))
	require.Equal(t, http.StatusOK, recAsset.Code)

	// Undeclared files, entrypoint, manifest, hidden files, and blocked extensions return 404.
	for _, blocked := range []string{"/script.js", "/komari-plugin.json", "/secret.env", "/.hidden"} {
		bRec := httptest.NewRecorder()
		r.ServeHTTP(bRec, httptest.NewRequest(http.MethodGet, "/api/admin/plugin/adm-page"+blocked, nil))
		require.Equalf(t, http.StatusNotFound, bRec.Code, "expected 404 for %s, got %d (%s)", blocked, bRec.Code, strings.TrimSpace(bRec.Body.String()))
	}
}
