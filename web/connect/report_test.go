package connectapi

import (
	"testing"

	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
)

func TestProtoReportBasicInfoPreservesTypedIdentity(t *testing.T) {
	report := &reportv1.AgentReport{
		System: &reportv1.SystemInfo{
			CpuName: "Example CPU", CpuCount: 8, CpuPhysicalCount: 4,
			Architecture: "amd64", Os: "Linux", KernelVersion: "6.8", Virtualization: "kvm", MemoryTotalBytes: 16 << 30,
		},
		Resources:         &reportv1.ResourceUsage{SwapTotalBytes: 2 << 30, Gpus: []*reportv1.GpuUsage{{Name: "GPU 0"}}},
		NetworkInterfaces: []*reportv1.NetworkInterface{{Addresses: []string{"192.0.2.10", "2001:db8::10"}}},
		Disks:             []*reportv1.DiskInfo{{TotalBytes: 100 << 30}},
		Metadata:          &reportv1.AgentMetadata{Version: "1.2.3"},
	}
	info := protoReportBasicInfo(report)
	if info["cpu_name"] != "Example CPU" || info["ipv4"] != "192.0.2.10" || info["ipv6"] != "2001:db8::10" || info["version"] != "1.2.3" {
		t.Fatalf("typed identity was not preserved: %#v", info)
	}
}

func TestProtoReportToLegacyPreservesCompleteRuntimeSurface(t *testing.T) {
	usage := 55.0
	report := protoReportToLegacy(&reportv1.AgentReport{
		Resources: &reportv1.ResourceUsage{
			ProcessCount: 12, TcpConnectionCount: 34, UdpConnectionCount: 56,
			Gpus: []*reportv1.GpuUsage{{Name: "GPU 0", UtilizationPercent: &usage}},
		},
		NetworkInterfaces: []*reportv1.NetworkInterface{{BytesSentPerSecond: 10, BytesReceivedPerSecond: 20}},
	})
	if report.Process != 12 || report.Connections.TCP != 34 || report.Connections.UDP != 56 || report.Network.Up != 10 || report.Network.Down != 20 || report.GPU == nil || report.GPU.AverageUsage != 55 {
		t.Fatalf("runtime report fields were lost: %+v", report)
	}
}
