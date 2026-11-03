package admin

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/upload"
	"github.com/stretchr/testify/require"
)

func admissionArchive(t *testing.T, short string) []byte {
	t.Helper()
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for name, body := range map[string]string{"komari-plugin.json": fmt.Sprintf(`{"short":%q,"name":"Pending fixture","version":"1"}`, short), "script.js": "throw new Error('staging must never execute this');"} {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return data.Bytes()
}

func TestPluginUploadAndChunkFinalizationStageWithoutExecution(t *testing.T) {
	require.NoError(t, plugin.CloseAll())
	oldDir, oldStorage := plugin.DataDir, plugin.StorageDir
	plugin.DataDir = t.TempDir()
	plugin.StorageDir = t.TempDir()
	t.Cleanup(func() { _ = plugin.CloseAll(); plugin.DataDir = oldDir; plugin.StorageDir = oldStorage })
	engine := gin.New()
	plugin.Init(engine)
	engine.Use(func(c *gin.Context) { api.SetPrincipal(c, rpc.NewUserPrincipal("human")); c.Next() })
	engine.POST("/upload", UploadPlugin)
	result := httptest.NewRecorder()
	engine.ServeHTTP(result, httptest.NewRequest("POST", "/upload", bytes.NewReader(admissionArchive(t, "direct-stage"))))
	require.Equal(t, 200, result.Code, result.Body.String())
	require.Empty(t, plugin.List())
	_, err := os.Stat(filepath.Join(plugin.DataDir, "direct-stage"))
	require.True(t, os.IsNotExist(err))
	path := filepath.Join(t.TempDir(), "chunked.zip")
	require.NoError(t, os.WriteFile(path, admissionArchive(t, "chunk-stage"), 0600))
	_, err = finalizePluginUpload(upload.Session{ArchivePath: path})
	require.NoError(t, err)
	require.Empty(t, plugin.List())
	rows, err := plugin.ListPackages()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, "pending", row.Status)
		require.Nil(t, row.ApprovedAt)
	}
}

func TestPluginUploadsAndCatalogInstallRejectProgrammaticPrincipals(t *testing.T) {
	for _, principal := range []*rpc.Principal{nil, rpc.NewAnonymousPrincipal(), rpc.NewAgentPrincipal("node"), rpc.NewAPIKeyPrincipal(), rpc.PrincipalFromRole(rpc.RoleAdmin)} {
		engine := gin.New()
		engine.Use(func(c *gin.Context) { api.SetPrincipal(c, principal); c.Next() })
		engine.POST("/upload", UploadPlugin)
		engine.POST("/market", InstallPluginFromMarket)
		for _, path := range []string{"/upload", "/market"} {
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
			require.Equal(t, 401, rec.Code, rec.Body.String())
		}
	}
}
