package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/config"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMarketDownloadSwitchControlsAllRemoteEntrypoints(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	config.SetDb(database)
	if enabled, err := ssrfProtectionEnabled(); err != nil || !enabled {
		t.Fatalf("historical settings lost default protection: %v %v", enabled, err)
	}
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/archive", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("archive"))
	}))
	defer server.Close()
	entrypoints := []struct {
		name     string
		download func(string) ([]byte, error)
	}{
		{"theme-import-update", downloadThemeFromURL},
		{"theme-market", func(raw string) ([]byte, error) { return downloadThemeMarketURL(raw, 128) }},
		{"plugin-market", func(raw string) ([]byte, error) { return downloadPluginMarketURL(raw, 128) }},
	}
	for _, protected := range []bool{true, false, true} {
		if err := config.Set(config.SSRFProtectionEnabledKey, protected); err != nil {
			t.Fatal(err)
		}
		for _, entry := range entrypoints {
			for _, path := range []string{"/archive", "/redirect"} {
				before := hits.Load()
				data, err := entry.download(server.URL + path)
				if protected {
					if err == nil || hits.Load() != before {
						t.Fatalf("%s reached local address while protected: %v", entry.name, err)
					}
				} else if err != nil || string(data) != "archive" {
					t.Fatalf("%s ignored explicit opt-out: %q %v", entry.name, data, err)
				}
			}
		}
	}
	if err := config.Set(config.SSRFProtectionEnabledKey, false); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadMarketURL(server.URL, 2, time.Second); !errors.Is(err, errMarketDownloadTooLarge) {
		t.Fatalf("opt-out removed size limit: %v", err)
	}
	if _, err := downloadMarketURL("file:///etc/passwd", 128, time.Second); err == nil {
		t.Fatal("opt-out allowed a non-HTTP scheme")
	}
	if err := database.Model(&config.ConfigItem{}).Where("key = ?", config.SSRFProtectionEnabledKey).Update("value", "invalid-json").Error; err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if _, err := downloadThemeFromURL(server.URL); err == nil || hits.Load() != before {
		t.Fatal("unreadable setting failed open")
	}
}
