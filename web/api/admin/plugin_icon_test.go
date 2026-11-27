package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/stretchr/testify/require"
)

func TestServePluginIconReadsOnlyDeclaredImagesForAdministrators(t *testing.T) {
	oldDir := plugin.DataDir
	plugin.DataDir = t.TempDir()
	t.Cleanup(func() { plugin.DataDir = oldDir })
	dir := filepath.Join(plugin.DataDir, "icon-fixture")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "art"), 0700))
	const icon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><path d="M0 0h16v16H0z"/></svg>`
	for name, contents := range map[string]string{
		"komari-plugin.json": `{"short":"icon-fixture","name":"Disabled icon fixture","version":"1","icon":"art/brand.svg","configuration":{"icon":"art/config.html"}}`,
		"art/brand.svg":      icon,
		"art/private.svg":    icon,
		"art/config.html":    "<h1>Configuration</h1>",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(contents), 0600))
	}
	principal := rpc.NewUserPrincipal("human")
	router := gin.New()
	router.Use(func(c *gin.Context) { api.SetPrincipal(c, principal); c.Next() })
	router.GET("/api/admin/plugin-icon/:short/*filepath", ServePluginIcon)
	get := func(name string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/plugin-icon/icon-fixture/"+name, nil))
		return rec
	}
	image := get("art/brand.svg")
	require.Equal(t, http.StatusOK, image.Code)
	require.Equal(t, icon, image.Body.String())
	require.Equal(t, "image/svg+xml", image.Header().Get("Content-Type"))
	require.Equal(t, "default-src 'none'; sandbox", image.Header().Get("Content-Security-Policy"))
	require.Equal(t, "nosniff", image.Header().Get("X-Content-Type-Options"))
	require.Equal(t, http.StatusNotFound, get("art/private.svg").Code)
	require.Equal(t, http.StatusNotFound, get("art/config.html").Code)
	principal = rpc.NewAnonymousPrincipal()
	require.Equal(t, http.StatusUnauthorized, get("art/brand.svg").Code)
	principal = rpc.NewAPITokenPrincipal(rpc.APITokenPolicy{})
	require.Equal(t, http.StatusUnauthorized, get("art/brand.svg").Code)
}
