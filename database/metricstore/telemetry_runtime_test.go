package metricstore

import (
	"testing"
	"time"
)

func TestTelemetryHotSafetyRetentionSettings(t *testing.T) {
	cfg, err := telemetryConfig(&MetricStoreConfig{})
	if err != nil || cfg.RawWindow != 15*time.Minute || cfg.RawRetention != 0 || cfg.MaxHotBytes != 16<<20 {
		t.Fatal("runtime must use raw SQL only for the bounded handoff queue", cfg, err)
	}
	for _, tc := range []struct {
		name              string
		online, retention int
		valid             bool
	}{
		{"short isolated fixture", 1, 1, true},
		{"normal one day", 15, 1440, true},
		{"maximum", 120, 10080, true},
		{"retired setting ignored", 120, 119, true},
		{"retired limit ignored", 15, 10081, true},
		{"retired negative ignored", 15, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := telemetryConfig(&MetricStoreConfig{HotWindowMinutes: tc.online, HotRetentionMinutes: tc.retention})
			if (err == nil) != tc.valid {
				t.Fatal(cfg, err)
			}
			if err == nil && (cfg.RawWindow != time.Duration(tc.online)*time.Minute || cfg.RawRetention != 0) {
				t.Fatal("setting lost", cfg)
			}
		})
	}
}
