package admin

import (
	"testing"

	"github.com/komari-monitor/komari/utils"
	"github.com/stretchr/testify/require"
)

func TestPluginMarketPackageAvailabilityUsesPanelVersionAndPackage(t *testing.T) {
	oldVersion := utils.CurrentVersion
	utils.CurrentVersion = "2.4.7"
	t.Cleanup(func() { utils.CurrentVersion = oldVersion })
	for _, tt := range []struct {
		name, constraint       string
		hasPackage, compatible bool
		installable            bool
	}{
		{"compatible bundle", ">=2.4.0", true, true, true},
		{"higher version required", ">=3.0.0", true, false, false},
		{"older version required", "<2.4.0", true, false, false},
		{"compatible listing without a bundle", "", false, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := PluginMarketPlugin{Komari: tt.constraint, Compatible: true, Installable: true}
			if tt.hasPackage {
				item.Download = "https://example.test/plugin.zip"
				item.SHA256 = "validated checksum"
			}
			item.setPackageAvailability()
			require.Equal(t, tt.compatible, item.Compatible)
			require.Equal(t, tt.installable, item.Installable)
		})
	}
	retired := PluginMarketPlugin{
		Short: "Komari-Passkey", Download: "https://example.test/plugin.zip",
		SHA256: "validated checksum",
	}
	retired.setPackageAvailability()
	require.True(t, retired.Retired)
	require.False(t, retired.Installable)
}
