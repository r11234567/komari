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

func TestMarketDownloadURLPatternValidation(t *testing.T) {
	validURLs := []string{
		"http://example.com",
		"https://example.com/theme.zip",
		"https://example.com:8080/path?query=val#hash",
		"http://127.0.0.1:8080/test",
		"https://[::1]:8443/archive",
		"https://api.github.com/repos/owner/repo/releases/latest",
		"https://github.com/owner/repo",
		"https://raw.githubusercontent.com/owner/repo/main/v1.json",
	}
	for _, raw := range validURLs {
		if !marketDownloadURLPattern.MatchString(raw) {
			t.Errorf("marketDownloadURLPattern rejected valid URL: %s", raw)
		}
	}

	invalidURLs := []string{
		"",
		"file:///etc/passwd",
		"ftp://example.com/file",
		"javascript:alert(1)",
		"data:text/html,test",
		"http://",
		"https://",
		"http://example.com/foo bar",
		"http://example.com\r\nHost: evil.com",
		"http://user:pass@example.com/",
	}
	for _, raw := range invalidURLs {
		if marketDownloadURLPattern.MatchString(raw) {
			t.Errorf("marketDownloadURLPattern accepted invalid URL: %s", raw)
		}
		if _, err := downloadMarketURL(raw, 1024, time.Second); err == nil {
			t.Errorf("downloadMarketURL allowed invalid URL: %s", raw)
		}
		if _, err := downloadThemeFromURL(raw); err == nil {
			t.Errorf("downloadThemeFromURL allowed invalid URL: %s", raw)
		}
	}
}

func TestGitHubSlugPatternValidation(t *testing.T) {
	validSlugs := []string{
		"owner",
		"repo-name",
		"repo.name",
		"repo_name",
		"12345",
		"Komari-Monitor",
	}
	for _, slug := range validSlugs {
		if !validGitHubSlugPattern.MatchString(slug) {
			t.Errorf("validGitHubSlugPattern rejected valid slug: %s", slug)
		}
	}

	invalidSlugs := []string{
		"",
		"../evil",
		"owner/repo",
		"owner\\repo",
		"owner;reboot",
		"owner$var",
		"owner repo",
		"owner\ntag",
	}
	for _, slug := range invalidSlugs {
		if validGitHubSlugPattern.MatchString(slug) {
			t.Errorf("validGitHubSlugPattern accepted invalid slug: %s", slug)
		}
	}
}
