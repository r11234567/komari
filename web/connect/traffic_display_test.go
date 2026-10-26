package connectapi

import (
	"github.com/komari-monitor/komari/database/trafficledger"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTrafficDisplayLeavesAgentCountersAndSpeedUnchanged(t *testing.T) {
	raw := &legacyv1.Report{Network: legacyv1.NetworkReport{TotalUp: 123, TotalDown: 456, Up: 7, Down: 8}}
	displayed := reportWithTrafficUsage(raw, map[string]trafficledger.Usage{"node": {Up: 500, Down: 600}}, "node")
	require.EqualValues(t, 123, raw.Network.TotalUp)
	require.EqualValues(t, 456, raw.Network.TotalDown)
	require.EqualValues(t, 500, displayed.Network.TotalUp)
	require.EqualValues(t, 600, displayed.Network.TotalDown)
	require.Equal(t, raw.Network.Up, displayed.Network.Up)
	require.Equal(t, raw.Network.Down, displayed.Network.Down)
	require.NotEqual(t, browserStatusVersion(&browserv1.AgentSummary{}, raw), browserStatusVersion(&browserv1.AgentSummary{}, displayed))
}
