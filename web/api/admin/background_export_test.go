package admin

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/historyexport"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
)

func TestBackgroundExportRequiresInteractiveAdministrator(t *testing.T) {
	for _, p := range []*rpc.Principal{rpc.NewAnonymousPrincipal(), rpc.NewAPIKeyPrincipal(), rpc.NewAgentPrincipal("node"), {Type: rpc.PrincipalUser, Roles: []string{rpc.RoleAdmin}}, {Type: rpc.PrincipalUser, UserUUID: "viewer"}} {
		r := gin.New()
		a := &backgroundExportAPI{}
		r.GET("/exports", func(c *gin.Context) {
			api.SetPrincipal(c, p)
			if _, ok := a.owner(c); ok {
				c.Status(200)
			}
		})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", "/exports", nil))
		if rec.Code != 403 {
			t.Fatalf("principal %+v admitted: %d", p, rec.Code)
		}
	}
}
func TestBackgroundExportCrossOwnerIsNotDiscoverable(t *testing.T) {
	q, e := historyexport.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer q.Close()
	j, e := q.Create(context.Background(), historyexport.Request{Owner: "alice", UUID: "node", Category: "resource", Mode: "raw", Start: time.Now().Add(-time.Hour), End: time.Now().Add(-time.Minute), Metrics: []string{"cpu.usage"}, Source: historyexport.Source{DSN: filepath.Join(t.TempDir(), "metrics.db"), Telemetry: metric.TelemetryConfig{ArchiveDirectory: t.TempDir()}}})
	if e != nil {
		t.Fatal(e)
	}
	a := &backgroundExportAPI{queue: q}
	for _, path := range []string{"/exports/:id", "/exports/:id/download", "/exports/:id/cancel"} {
		r := gin.New()
		r.Use(func(c *gin.Context) { api.SetPrincipal(c, rpc.NewUserPrincipal("bob")); c.Next() })
		r.GET(path, a.get)
		actual := "/exports/" + j.ID
		if path == "/exports/:id/download" {
			actual += "/download"
		}
		if path == "/exports/:id/cancel" {
			actual += "/cancel"
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", actual, nil))
		if rec.Code != 404 {
			t.Fatal("cross-account task disclosed", rec.Code, rec.Body.String())
		}
	}
}
func TestBackgroundExportSourceRejectsUnisolatedAndMemoryDatabases(t *testing.T) {
	for _, cfg := range []metricstore.MetricStoreConfig{{Driver: "sqlite", DSN: ":memory:"}, {Driver: "sqlite", DSN: "file:temp?mode=memory&cache=shared"}, {Driver: "postgres", DSN: "postgres://example/db"}, {Driver: "mysql", DSN: "user@tcp(example)/db"}} {
		if _, e := backgroundSource(&cfg); e == nil {
			t.Fatal("unsafe source accepted", cfg.Driver, cfg.DSN)
		}
	}
	source, e := backgroundSource(&metricstore.MetricStoreConfig{Driver: "sqlite", DSN: "file:./data/metrics.db?_busy_timeout=5000"})
	if e != nil || !filepath.IsAbs(source.DSN) || !filepath.IsAbs(source.Telemetry.ArchiveDirectory) {
		t.Fatal(source, e)
	}
}
