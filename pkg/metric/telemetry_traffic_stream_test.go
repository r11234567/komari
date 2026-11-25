package metric

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMeasuredTrafficHandlesHostRebootEvenWhenCountersGrew(t *testing.T) {
	before := time.Now().UTC().Add(-time.Minute)
	prior := TrafficSample{Observed: before.UnixNano(), BootID: "boot1", Interfaces: map[string]TrafficCounters{"eth0": {Sent: 100, Received: 15}}}
	sample := TrafficSample{Observed: before.Add(time.Minute).UnixNano(), BootID: "boot2", Booted: before.Add(time.Second).UnixNano(), Interfaces: map[string]TrafficCounters{"eth0": {Sent: 250, Received: 50}, "eth1": {Sent: 900}}}
	result, err := measuredTraffic(prior, sample, true)
	require.NoError(t, err)
	require.EqualValues(t, 250, result.DeltaUp)
	require.EqualValues(t, 50, result.DeltaDown)
	require.Equal(t, sample.Booted, result.Start)
	require.Equal(t, prior.Observed, result.GapStart)
	sample.BootID = prior.BootID
	result, err = measuredTraffic(prior, sample, true)
	require.NoError(t, err)
	require.EqualValues(t, 150, result.DeltaUp)
	require.EqualValues(t, 35, result.DeltaDown)
	require.Zero(t, result.GapStart)
}
