package connectapi

import (
	"testing"

	"github.com/komari-monitor/komari/database/clients"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	"github.com/stretchr/testify/require"
)

func TestWindowsBasicGPUUsesOrdinaryConfigAndPreservesFalse(t *testing.T) {
	for _, platform := range []string{"windows", "linux", "macos"} {
		t.Run(platform, func(t *testing.T) {
			profile := clients.DeploymentProfile{Platform: platform}
			require.Nil(t, runtimeToProto(profile).EnableGpu)
			for _, enabled := range []bool{false, true, false} {
				err := applyRuntime(&profile, &configv1.RuntimeConfig{EnableGpu: &enabled})
				if platform != "windows" {
					require.Error(t, err)
					require.Nil(t, profile.WindowsGPUEnabled)
					continue
				}
				require.NoError(t, err)
				require.Equal(t, enabled, runtimeToProto(profile).GetEnableGpu())
				require.False(t, profile.EnableGPU, "ordinary toggle changed the installation grant")
			}
		})
	}
}
