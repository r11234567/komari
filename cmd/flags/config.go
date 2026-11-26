package flags

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/idna"
)

const (
	DatabaseTypeSQLite = "sqlite"
)

var (
	// 数据库配置
	DatabaseType string // 数据库类型：sqlite
	DatabaseFile string // SQLite数据库文件路径

	Listen       string
	PanelListen  string
	PanelOrigin  string
	PublicOrigin string
)

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// EffectivePanelListen returns the configured dedicated panel listener address,
// reading KOMARI_PANEL_LISTEN when the CLI flag was not set explicitly.
func EffectivePanelListen() string {
	return firstNonEmptyTrimmed(PanelListen, os.Getenv("KOMARI_PANEL_LISTEN"))
}

// SplitPanelEnabled reports whether dual-listener physical route isolation is enabled.
func SplitPanelEnabled() bool {
	return EffectivePanelListen() != ""
}

// SharedOriginThemesEnabled is an operator opt-in to running installed themes
// with the browser privileges of the management origin.
func SharedOriginThemesEnabled() bool {
	return !SplitPanelEnabled() && strings.EqualFold(strings.TrimSpace(os.Getenv("KOMARI_ALLOW_SHARED_ORIGIN_THEMES")), "true")
}

// ControlListenAddr returns the listener address where management and setup
// ceremonies (install guide, legacy upgrade, storage migration) must bind.
func ControlListenAddr() string {
	if SplitPanelEnabled() {
		return EffectivePanelListen()
	}
	return strings.TrimSpace(Listen)
}

// NormalizeOriginConfig validates and canonicalizes an external http(s) origin
// (scheme://host[:port]) without path, query, fragment, or userinfo.
func NormalizeOriginConfig(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid origin %q: %w", raw, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("origin %q must use http or https scheme", raw)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("origin %q must contain only scheme and host", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("origin %q must not include a path (%q)", raw, parsed.Path)
	}
	return scheme + "://" + strings.ToLower(parsed.Host), nil
}

// EffectivePublicOrigin returns the normalized KOMARI_PUBLIC_ORIGIN if configured.
func EffectivePublicOrigin() string {
	normalized, err := NormalizeOriginConfig(firstNonEmptyTrimmed(PublicOrigin, os.Getenv("KOMARI_PUBLIC_ORIGIN")))
	if err != nil {
		return ""
	}
	return normalized
}

// EffectivePanelOrigin returns the normalized KOMARI_PANEL_ORIGIN, falling back
// to KOMARI_PUBLIC_ORIGIN only in single-port combined mode.
func EffectivePanelOrigin() string {
	rawPanel := firstNonEmptyTrimmed(PanelOrigin, os.Getenv("KOMARI_PANEL_ORIGIN"))
	if rawPanel != "" {
		normalized, err := NormalizeOriginConfig(rawPanel)
		if err != nil {
			return ""
		}
		return normalized
	}
	if !SplitPanelEnabled() {
		return EffectivePublicOrigin()
	}
	return ""
}

// ValidateListenerConfig checks listener addresses and external origins before startup.
func ValidateListenerConfig() error {
	if rawPublic := firstNonEmptyTrimmed(PublicOrigin, os.Getenv("KOMARI_PUBLIC_ORIGIN")); rawPublic != "" {
		if _, err := NormalizeOriginConfig(rawPublic); err != nil {
			return fmt.Errorf("KOMARI_PUBLIC_ORIGIN: %w", err)
		}
	}
	if rawPanel := firstNonEmptyTrimmed(PanelOrigin, os.Getenv("KOMARI_PANEL_ORIGIN")); rawPanel != "" {
		if _, err := NormalizeOriginConfig(rawPanel); err != nil {
			return fmt.Errorf("KOMARI_PANEL_ORIGIN: %w", err)
		}
	}
	panelAddr := EffectivePanelListen()
	if panelAddr == "" {
		return nil
	}
	publicOrigin, panelOrigin := EffectivePublicOrigin(), EffectivePanelOrigin()
	if publicOrigin == "" || panelOrigin == "" {
		return fmt.Errorf("split listeners require both KOMARI_PUBLIC_ORIGIN and KOMARI_PANEL_ORIGIN")
	}
	publicHost, err := originHostname(publicOrigin)
	if err != nil {
		return fmt.Errorf("KOMARI_PUBLIC_ORIGIN: %w", err)
	}
	panelOriginHost, err := originHostname(panelOrigin)
	if err != nil {
		return fmt.Errorf("KOMARI_PANEL_ORIGIN: %w", err)
	}
	if publicHost == panelOriginHost {
		return fmt.Errorf("KOMARI_PUBLIC_ORIGIN and KOMARI_PANEL_ORIGIN must use distinct hostnames for cookie isolation")
	}
	panelHost, panelPort, err := net.SplitHostPort(panelAddr)
	if err != nil {
		return fmt.Errorf("KOMARI_PANEL_LISTEN %q: %w", panelAddr, err)
	}
	publicAddr := strings.TrimSpace(Listen)
	if publicAddr != "" {
		pubHost, pubPort, pubErr := net.SplitHostPort(publicAddr)
		if pubErr == nil && panelPort != "0" && panelPort == pubPort && canonicalListenHost(panelHost) == canonicalListenHost(pubHost) {
			return fmt.Errorf("KOMARI_PANEL_LISTEN (%s) collides with KOMARI_LISTEN (%s)", panelAddr, publicAddr)
		}
	}
	return nil
}

func originHostname(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if host == "" {
		return "", fmt.Errorf("empty origin hostname")
	}
	ascii, err := idna.Lookup.ToASCII(host)
	return strings.ToLower(ascii), err
}

func canonicalListenHost(host string) string {
	trimmed := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	switch trimmed {
	case "", "0.0.0.0", "::":
		return "*"
	default:
		return trimmed
	}
}

func NormalizeDatabaseType(databaseType string) string {
	databaseType = strings.ToLower(strings.TrimSpace(databaseType))
	if databaseType == "" {
		return DatabaseTypeSQLite
	}
	return databaseType
}

func ApplyDatabaseTypeNormalization() string {
	DatabaseType = NormalizeDatabaseType(DatabaseType)
	return DatabaseType
}

func IsSQLite() bool {
	return NormalizeDatabaseType(DatabaseType) == DatabaseTypeSQLite
}

func SupportedDatabaseTypes() string {
	return DatabaseTypeSQLite
}

// NativePasskeyConfigured selects the native store even if RP settings are incomplete.
func NativePasskeyConfigured() bool {
	return strings.TrimSpace(os.Getenv("KOMARI_PASSKEY_ORIGIN")) != "" ||
		strings.TrimSpace(os.Getenv("KOMARI_PASSKEY_PANEL_ORIGIN")) != "" ||
		strings.TrimSpace(os.Getenv("KOMARI_PASSKEY_RP_ID")) != ""
}
