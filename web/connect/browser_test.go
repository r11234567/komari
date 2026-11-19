package connectapi

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/models"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
)

func TestMaskGuestIPMatchesLegacyCompatibilityShape(t *testing.T) {
	ipv4, ipv6 := maskGuestIP("203.0.113.18", "2001:db8:1:2:3:4:5:6")
	if ipv4 != "203.*.*.*" || ipv6 != "2001:*:*:*:*:*:*:*" {
		t.Fatalf("unexpected masked addresses: ipv4=%q ipv6=%q", ipv4, ipv6)
	}
}

func TestBrowserBasicInfoUsesActiveTrafficQuota(t *testing.T) {
	info := browserBasicInfo(models.Client{
		TrafficLimit:          100,
		TrafficLimitType:      "max",
		EffectiveTrafficLimit: 150,
		EffectiveTrafficType:  "down",
	}, false, false)
	if info.TrafficLimitBytes != 150 || info.TrafficLimitType != "down" {
		t.Fatalf("unexpected active quota: bytes=%d type=%q", info.TrafficLimitBytes, info.TrafficLimitType)
	}
}

func TestStructValueAcceptsGinThemeSettings(t *testing.T) {
	settings, err := structValue(map[string]interface{}{
		"theme_settings": gin.H{"compact": true},
	}, "theme_settings")
	if err != nil {
		t.Fatalf("convert theme settings: %v", err)
	}
	if value, ok := settings.Fields["compact"]; !ok || !value.GetBoolValue() {
		t.Fatal("gin.H theme setting was not preserved")
	}
}

func TestLegacyReportToProtoPreservesGPUAndDiagnosticMessage(t *testing.T) {
	client := models.Client{
		UUID:    "test-client-uuid",
		Name:    "test-client",
		OS:      "linux",
		GpuName: "NVIDIA GeForce RTX 4090",
	}
	report := &legacyv1.Report{
		Message: "collector warning: sample test message",
		CPU:     legacyv1.CPUReport{Usage: 12.5},
		GPU: &legacyv1.GPUDetailReport{
			Count:        2,
			AverageUsage: 65.0,
			DetailedInfo: []legacyv1.GPUDeviceInfo{
				{
					Name:        "NVIDIA GeForce RTX 4090",
					Utilization: 70.0,
					MemoryUsed:  4 << 30,
					MemoryTotal: 24 << 30,
					Temperature: 65,
				},
				{
					Name:        "NVIDIA GeForce RTX 4080",
					Utilization: 60.0,
					MemoryUsed:  2 << 30,
					MemoryTotal: 16 << 30,
					Temperature: 58,
				},
			},
		},
	}

	protoReport := legacyReportToProto(client, report, true)
	if protoReport == nil {
		t.Fatal("expected non-nil proto report")
	}
	if protoReport.DiagnosticMessage != "collector warning: sample test message" {
		t.Fatalf("unexpected diagnostic message: %q", protoReport.DiagnosticMessage)
	}
	if protoReport.Resources == nil || len(protoReport.Resources.Gpus) != 2 {
		t.Fatalf("expected 2 GPUs in proto report, got: %#v", protoReport.Resources)
	}
	gpu0 := protoReport.Resources.Gpus[0]
	if gpu0.Id != "0" || gpu0.Name != "NVIDIA GeForce RTX 4090" || gpu0.GetUtilizationPercent() != 70.0 ||
		gpu0.GetMemoryUsedBytes() != 4<<30 || gpu0.GetMemoryTotalBytes() != 24<<30 || gpu0.GetTemperatureCelsius() != 65 {
		t.Fatalf("unexpected gpu0 in proto: %+v", gpu0)
	}
	gpu1 := protoReport.Resources.Gpus[1]
	if gpu1.Id != "1" || gpu1.Name != "NVIDIA GeForce RTX 4080" || gpu1.GetUtilizationPercent() != 60.0 ||
		gpu1.GetMemoryUsedBytes() != 2<<30 || gpu1.GetMemoryTotalBytes() != 16<<30 || gpu1.GetTemperatureCelsius() != 58 {
		t.Fatalf("unexpected gpu1 in proto: %+v", gpu1)
	}
}

func TestLegacyReportToProtoRoundTripWithProtoReportToLegacy(t *testing.T) {
	usage0 := 45.5
	usage1 := 75.0
	memUsed0 := uint64(2 << 30)
	memTotal0 := uint64(8 << 30)
	memUsed1 := uint64(6 << 30)
	memTotal1 := uint64(12 << 30)
	temp0 := 52.0
	temp1 := 68.0

	origProto := &reportv1.AgentReport{
		AgentId:           "agent-roundtrip",
		DiagnosticMessage: "probe ok",
		Resources: &reportv1.ResourceUsage{
			CpuPercent: 20.0,
			Gpus: []*reportv1.GpuUsage{
				{
					Id:                 "0",
					Name:               "GPU Alpha",
					UtilizationPercent: &usage0,
					MemoryUsedBytes:    &memUsed0,
					MemoryTotalBytes:   &memTotal0,
					TemperatureCelsius: &temp0,
				},
				{
					Id:                 "1",
					Name:               "GPU Beta",
					UtilizationPercent: &usage1,
					MemoryUsedBytes:    &memUsed1,
					MemoryTotalBytes:   &memTotal1,
					TemperatureCelsius: &temp1,
				},
			},
		},
	}

	legacy := protoReportToLegacy(origProto)
	if legacy.Message != "probe ok" {
		t.Fatalf("unexpected legacy message: %q", legacy.Message)
	}
	if legacy.GPU == nil || len(legacy.GPU.DetailedInfo) != 2 {
		t.Fatalf("expected 2 legacy detailed info items: %+v", legacy.GPU)
	}

	client := models.Client{UUID: "agent-roundtrip"}
	convertedProto := legacyReportToProto(client, &legacy, false)

	if convertedProto.DiagnosticMessage != "probe ok" {
		t.Fatalf("diagnostic message lost in round-trip: %q", convertedProto.DiagnosticMessage)
	}
	if len(convertedProto.Resources.Gpus) != 2 {
		t.Fatalf("expected 2 gpus after round-trip, got: %d", len(convertedProto.Resources.Gpus))
	}
	for i, orig := range origProto.Resources.Gpus {
		conv := convertedProto.Resources.Gpus[i]
		if conv.Name != orig.Name || conv.GetUtilizationPercent() != orig.GetUtilizationPercent() ||
			conv.GetMemoryUsedBytes() != orig.GetMemoryUsedBytes() || conv.GetMemoryTotalBytes() != orig.GetMemoryTotalBytes() ||
			conv.GetTemperatureCelsius() != orig.GetTemperatureCelsius() {
			t.Fatalf("gpu %d mismatch after round trip: conv=%+v orig=%+v", i, conv, orig)
		}
	}
}

func TestLegacyReportToProtoHandlesNilGPUGracefully(t *testing.T) {
	client := models.Client{UUID: "client-no-gpu"}
	report := &legacyv1.Report{
		CPU: legacyv1.CPUReport{Usage: 5.0},
	}
	protoReport := legacyReportToProto(client, report, false)
	if protoReport == nil {
		t.Fatal("expected non-nil proto report")
	}
	if len(protoReport.Resources.Gpus) != 0 {
		t.Fatalf("expected empty gpus for nil GPU report, got: %+v", protoReport.Resources.Gpus)
	}
}
