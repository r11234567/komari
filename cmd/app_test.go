package cmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/config"
	installweb "github.com/komari-monitor/komari/web/install"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestFirstRunInstallRedirect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(firstRunInstallRedirect())
	router.Any("/*path", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	tests := []struct {
		name         string
		method       string
		path         string
		wantStatus   int
		wantLocation string
	}{
		{name: "root", method: http.MethodGet, path: "/", wantStatus: http.StatusTemporaryRedirect, wantLocation: installweb.PagePath},
		{name: "public route", method: http.MethodGet, path: "/instance/example", wantStatus: http.StatusTemporaryRedirect, wantLocation: installweb.PagePath},
		{name: "admin route", method: http.MethodGet, path: "/admin", wantStatus: http.StatusTemporaryRedirect, wantLocation: installweb.PagePath},
		{name: "install page", method: http.MethodGet, path: installweb.PagePath, wantStatus: http.StatusNoContent},
		{name: "install API", method: http.MethodGet, path: installweb.APIPath + "/status", wantStatus: http.StatusNoContent},
		{name: "system asset", method: http.MethodGet, path: "/system-assets/assets/entry.js", wantStatus: http.StatusNoContent},
		{name: "favicon", method: http.MethodGet, path: "/favicon.ico", wantStatus: http.StatusNoContent},
		{name: "non GET request", method: http.MethodPost, path: "/", wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, nil)
			router.ServeHTTP(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if location := recorder.Header().Get("Location"); location != tt.wantLocation {
				t.Fatalf("Location = %q, want %q", location, tt.wantLocation)
			}
		})
	}
}

func TestNormalizeMetricStorageSettingsOverridesLegacyValues(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:normalize-metric-settings?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open config database: %v", err)
	}
	config.SetDb(db)
	if err := config.SetMany(map[string]any{
		config.LowResourceModeKey:                true,
		metricstore.MetricDownsamplingEnabledKey: false,
		"metric_retention_days":                  37,
	}); err != nil {
		t.Fatalf("seed legacy settings: %v", err)
	}
	if err := normalizeMetricStorageSettings(); err != nil {
		t.Fatalf("normalize storage settings: %v", err)
	}
	lowResource, err := config.GetAs[bool](config.LowResourceModeKey)
	if err != nil || lowResource {
		t.Fatalf("low resource mode = %t, err %v; want false", lowResource, err)
	}
	downsampling, err := config.GetAs[bool](metricstore.MetricDownsamplingEnabledKey)
	if err != nil || downsampling {
		t.Fatalf("downsampling = %t, err %v; want preserved false", downsampling, err)
	}
	retention, err := config.GetAs[int]("metric_retention_days")
	if err != nil || retention != 37 {
		t.Fatalf("retention = %d, err %v; want preserved value 37", retention, err)
	}
}

func TestMetricCompactCycleStateConcurrentAddAndReset(t *testing.T) {
	const workers = 64
	wantErr := errors.New("compact step failed")
	var state metricCompactCycleState
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func(failed bool) {
			defer group.Done()
			var err error
			if failed {
				err = wantErr
			}
			state.add(1, err)
		}(i%2 == 0)
	}
	group.Wait()

	written, err := state.finish()
	if written != workers {
		t.Fatalf("cycle buckets = %d, want %d", written, workers)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("cycle error = %v, want joined compact error", err)
	}
	if written, err := state.finish(); written != 0 || err != nil {
		t.Fatalf("reset cycle = buckets %d, err %v; want zero state", written, err)
	}
}

func TestSplitListenersStartServeAndShutdownWithoutPortLeak(t *testing.T) {
	if os.Getenv("KOMARI_TEST_SPLIT_LISTENERS") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSplitListenersStartServeAndShutdownWithoutPortLeak$", "-test.v")
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "KOMARI_TEST_SPLIT_LISTENERS=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("split listener subprocess failed: %v\n%s", err, out)
		}
		return
	}

	gin.SetMode(gin.TestMode)

	origListen, origPanel, origPanelOrigin, origPublicOrigin, origDBFile, origDBType := flags.Listen, flags.PanelListen, flags.PanelOrigin, flags.PublicOrigin, flags.DatabaseFile, flags.DatabaseType
	t.Cleanup(func() {
		flags.Listen, flags.PanelListen, flags.PanelOrigin, flags.PublicOrigin, flags.DatabaseFile, flags.DatabaseType = origListen, origPanel, origPanelOrigin, origPublicOrigin, origDBFile, origDBType
	})

	flags.DatabaseType = "sqlite"
	flags.DatabaseFile = "./data/komari.db"
	flags.Listen = "127.0.0.1:0"
	flags.PanelListen = "127.0.0.1:0"
	flags.PublicOrigin = "https://status.example.com"
	flags.PanelOrigin = "https://panel.example.com"

	app := NewApp()
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := app.BuildRouter(); err != nil {
		_ = app.Shutdown()
		t.Fatalf("BuildRouter: %v", err)
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- app.Run()
	}()

	var pubAddr, panAddr string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pubAddr = app.PublicAddr()
		panAddr = app.PanelAddr()
		if pubAddr != "" && panAddr != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pubAddr == "" || panAddr == "" || pubAddr == panAddr {
		_ = app.Shutdown()
		t.Fatalf("listeners failed to bind distinct addresses: public=%q panel=%q", pubAddr, panAddr)
	}

	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Public listener serves /ping (200) and rejects /admin and /api/admin/* (404).
	resp, err := client.Get("http://" + pubAddr + "/ping")
	if err != nil || resp.StatusCode != http.StatusOK {
		_ = app.Shutdown()
		t.Fatalf("public /ping = %v, err=%v", resp, err)
	}
	_ = resp.Body.Close()

	for _, path := range []string{"/admin", "/api/admin/settings/"} {
		resp, err = client.Get("http://" + pubAddr + path)
		if err != nil || resp.StatusCode != http.StatusNotFound {
			_ = app.Shutdown()
			t.Fatalf("public %s = %v, err=%v; want 404", path, resp, err)
		}
		_ = resp.Body.Close()
	}

	// Panel listener serves /admin (200) and rejects /api/rpc2 and /themes/* (404).
	resp, err = client.Get("http://" + panAddr + "/admin")
	if err != nil || resp.StatusCode != http.StatusOK {
		_ = app.Shutdown()
		t.Fatalf("panel /admin = %v, err=%v; want 200", resp, err)
	}
	_ = resp.Body.Close()

	for _, path := range []string{"/api/rpc2", "/themes/default/komari-theme.json"} {
		resp, err = client.Get("http://" + panAddr + path)
		if err != nil || resp.StatusCode != http.StatusNotFound {
			_ = app.Shutdown()
			t.Fatalf("panel %s = %v, err=%v; want 404", path, resp, err)
		}
		_ = resp.Body.Close()
	}

	// Establish live upgraded WebSocket connections on /api/rpc2 and /api/clients
	// to prove Shutdown actively closes hijacked WebSocket connections too.
	wsHeaders := http.Header{"Origin": []string{"http://" + pubAddr}}
	rpcWS, _, err := websocket.DefaultDialer.Dial("ws://"+pubAddr+"/api/rpc2", wsHeaders)
	if err != nil {
		_ = app.Shutdown()
		t.Fatalf("dial /api/rpc2 websocket: %v", err)
	}
	defer rpcWS.Close()
	if err := rpcWS.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":1,"method":"rpc.ping"}`)); err != nil {
		_ = app.Shutdown()
		t.Fatalf("write /api/rpc2 websocket: %v", err)
	}
	if _, _, err := rpcWS.ReadMessage(); err != nil {
		_ = app.Shutdown()
		t.Fatalf("read /api/rpc2 websocket: %v", err)
	}

	clientsWS, _, err := websocket.DefaultDialer.Dial("ws://"+pubAddr+"/api/clients", wsHeaders)
	if err != nil {
		_ = app.Shutdown()
		t.Fatalf("dial /api/clients websocket: %v", err)
	}
	defer clientsWS.Close()
	if err := clientsWS.WriteMessage(websocket.TextMessage, []byte("get")); err != nil {
		_ = app.Shutdown()
		t.Fatalf("write /api/clients websocket: %v", err)
	}
	if _, _, err := clientsWS.ReadMessage(); err != nil {
		_ = app.Shutdown()
		t.Fatalf("read /api/clients websocket: %v", err)
	}

	// Graceful Shutdown closes active WebSocket connections, releases both sockets, and exits Run cleanly.
	if err := app.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for name, ws := range map[string]*websocket.Conn{"/api/rpc2": rpcWS, "/api/clients": clientsWS} {
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := ws.ReadMessage(); err == nil {
			t.Fatalf("upgraded websocket %s remained open after Shutdown", name)
		}
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error after Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after Shutdown")
	}

	for _, addr := range []string{pubAddr, panAddr} {
		rebind, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("socket %s remained bound after Shutdown: %v", addr, err)
		}
		_ = rebind.Close()
	}
}
