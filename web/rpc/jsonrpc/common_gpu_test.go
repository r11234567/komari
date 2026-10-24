package jsonrpc

import (
	"testing"

	v1 "github.com/komari-monitor/komari/protocol/v1"
)

func TestGpuUsageFromReport(t *testing.T) {
	if gpuUsageFromReport(nil) != 0 {
		t.Fatalf("nil report should be 0")
	}
	if gpuUsageFromReport(&v1.Report{}) != 0 {
		t.Fatalf("missing GPU should be 0")
	}

	got := gpuUsageFromReport(&v1.Report{
		GPU: &v1.GPUDetailReport{
			Count:        1,
			AverageUsage: 87.5,
			DetailedInfo: []v1.GPUDeviceInfo{{
				Name:        "Phoenix1",
				Utilization: 87.5,
			}},
		},
	})
	if got != 87.5 {
		t.Fatalf("got %v, want 87.5", got)
	}
}
