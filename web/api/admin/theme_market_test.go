package admin

import (
	"archive/zip"
	"net"
	"testing"
)

func TestParseThemeMarketCatalogShapes(t *testing.T) {
	theme := `{"name":"Test","short":"Test","version":"1.0.0","author":"Author","download":"https://example.com/theme.zip","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	tests := []string{
		theme,
		`[` + theme + `]`,
		`{"schema":1,"themes":[` + theme + `]}`,
	}
	for _, input := range tests {
		themes, err := parseThemeMarketCatalog([]byte(input))
		if err != nil {
			t.Fatalf("parseThemeMarketCatalog() error = %v", err)
		}
		if len(themes) != 1 || themes[0].Short != "Test" {
			t.Fatalf("parseThemeMarketCatalog() = %#v", themes)
		}
	}
}

func TestValidateThemeMarketThemeChecksum(t *testing.T) {
	valid := ThemeMarketTheme{
		Name: "Test", Short: "Test", Version: "1.0.0", Author: "Author",
		URL:      "https://example.com/theme",
		Download: "https://example.com/theme.zip",
		SHA256:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if err := validateThemeMarketTheme(valid); err != nil {
		t.Fatalf("validateThemeMarketTheme() error = %v", err)
	}
	valid.SHA256 = "xxxxxx"
	if err := validateThemeMarketTheme(valid); err == nil {
		t.Fatal("validateThemeMarketTheme() accepted an invalid checksum")
	}
}

func TestValidateThemeArchiveLimits(t *testing.T) {
	files := make([]*zip.File, maxThemeArchiveFiles+1)
	for i := range files {
		files[i] = &zip.File{}
	}
	if err := validateThemeArchive(files); err == nil {
		t.Fatal("validateThemeArchive() accepted too many files")
	}

	large := &zip.File{FileHeader: zip.FileHeader{UncompressedSize64: maxThemeFileSize + 1}}
	if err := validateThemeArchive([]*zip.File{large}); err == nil {
		t.Fatal("validateThemeArchive() accepted an oversized file")
	}
}

func TestThemeMarketI18nTextAndSourceOnlyEntry(t *testing.T) {
	theme := ThemeMarketTheme{
		Name:    map[string]any{"zh-CN": "测试", "en": "Test"},
		Short:   "source-only",
		Version: "source",
		Author:  map[string]any{"en": "Author"},
		URL:     "https://example.com/theme",
	}
	if err := validateThemeMarketTheme(theme); err != nil {
		t.Fatalf("validateThemeMarketTheme() error = %v", err)
	}
}

func TestMarketDownloadRejectsInternalAddresses(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"100.64.0.1",
		"169.254.169.254",
		"192.0.2.1",
		"::1",
		"fc00::1",
		"2001:db8::1",
	} {
		if isPublicMarketIP(net.ParseIP(raw)) {
			t.Fatalf("internal or reserved address %s was accepted", raw)
		}
	}
	if !isPublicMarketIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("public address was rejected")
	}
}

func TestValidateMarketDownloadURL(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"https://user@example.com/theme.zip",
		"https://[fe80::1%25eth0]/theme.zip",
	} {
		if err := validateMarketDownloadURL(raw); err == nil {
			t.Fatalf("unsafe URL %q was accepted", raw)
		}
	}
	if err := validateMarketDownloadURL("https://example.com/theme.zip"); err != nil {
		t.Fatalf("public HTTPS URL was rejected: %v", err)
	}
}
