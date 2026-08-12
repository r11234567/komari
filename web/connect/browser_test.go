package connectapi

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/models"
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
