package flags

import "testing"

func TestNormalizeDatabaseType(t *testing.T) {
	tests := map[string]string{
		"":          DatabaseTypeSQLite,
		"sqlite":    DatabaseTypeSQLite,
		" SQLite ":  DatabaseTypeSQLite,
		"SQLITE":    DatabaseTypeSQLite,
		"postgres":  "postgres",
		" postgres": "postgres",
	}

	for input, want := range tests {
		if got := NormalizeDatabaseType(input); got != want {
			t.Fatalf("NormalizeDatabaseType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPanelListenerAndOriginNormalization(t *testing.T) {
	origListen, origPanel, origPanelOrigin, origPublicOrigin := Listen, PanelListen, PanelOrigin, PublicOrigin
	t.Cleanup(func() {
		Listen, PanelListen, PanelOrigin, PublicOrigin = origListen, origPanel, origPanelOrigin, origPublicOrigin
	})

	Listen = "127.0.0.1:25774"
	PanelListen = ""
	PanelOrigin = ""
	PublicOrigin = ""

	t.Setenv("KOMARI_PUBLIC_ORIGIN", "HTTPS://Status.Example.com/")
	t.Setenv("KOMARI_PANEL_ORIGIN", "")
	t.Setenv("KOMARI_PANEL_LISTEN", "")

	if SplitPanelEnabled() {
		t.Fatal("SplitPanelEnabled() = true when KOMARI_PANEL_LISTEN is empty")
	}
	if got := EffectivePublicOrigin(); got != "https://status.example.com" {
		t.Fatalf("EffectivePublicOrigin() = %q, want https://status.example.com", got)
	}
	// Single-port mode falls back to KOMARI_PUBLIC_ORIGIN when KOMARI_PANEL_ORIGIN is unset.
	if got := EffectivePanelOrigin(); got != "https://status.example.com" {
		t.Fatalf("single-port EffectivePanelOrigin() = %q, want https://status.example.com", got)
	}

	// Split mode does not fall back to KOMARI_PUBLIC_ORIGIN.
	t.Setenv("KOMARI_PANEL_LISTEN", "127.0.0.1:25775")
	if !SplitPanelEnabled() {
		t.Fatal("SplitPanelEnabled() = false when KOMARI_PANEL_LISTEN is set")
	}
	if got := EffectivePanelOrigin(); got != "" {
		t.Fatalf("split EffectivePanelOrigin() without KOMARI_PANEL_ORIGIN = %q, want empty", got)
	}
	t.Setenv("KOMARI_PANEL_ORIGIN", "https://panel.example.com/")
	if got := EffectivePanelOrigin(); got != "https://panel.example.com" {
		t.Fatalf("split EffectivePanelOrigin() = %q, want https://panel.example.com", got)
	}
	if err := ValidateListenerConfig(); err != nil {
		t.Fatalf("ValidateListenerConfig() unexpected error: %v", err)
	}

	// Colliding addresses must fail validation.
	t.Setenv("KOMARI_PANEL_LISTEN", "127.0.0.1:25774")
	if err := ValidateListenerConfig(); err == nil {
		t.Fatal("ValidateListenerConfig() accepted colliding Listen and PanelListen")
	}

	// Origins with paths or query strings must fail validation.
	t.Setenv("KOMARI_PANEL_LISTEN", "127.0.0.1:25775")
	t.Setenv("KOMARI_PANEL_ORIGIN", "https://panel.example.com/subpath")
	if err := ValidateListenerConfig(); err == nil {
		t.Fatal("ValidateListenerConfig() accepted origin with non-root path")
	}
}
