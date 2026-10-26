package upload

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
)

func TestArchiveEndpointsRequireInteractiveSession(t *testing.T) {
	for _, principal := range []*rpc.Principal{nil, rpc.NewAnonymousPrincipal(), rpc.NewAgentPrincipal("node"), rpc.NewAPIKeyPrincipal(), rpc.NewUserPrincipal("user-without-session")} {
		h := NewHandler(NewStore(t.TempDir()), nil)
		for _, handler := range []gin.HandlerFunc{h.Init, h.Chunk, h.Merge, h.Cancel} {
			r := gin.New()
			r.POST("/upload", func(c *gin.Context) { api.SetPrincipal(c, principal); handler(c) })
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upload", nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("principal %v: %d", principal, rec.Code)
			}
		}
	}
}
