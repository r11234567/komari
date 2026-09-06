package admin

import (
	"net"
	"testing"
)

// blockedThemeAddress is the last line of defence for market/theme downloads:
// it judges the address actually being dialled, after DNS has resolved. Ranges
// that Go's own predicates call global unicast still reach infrastructure, and
// CGNAT is where cloud providers put internal endpoints.
func TestBlockedThemeAddressRejectsNonPublicRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", // cloud metadata
		"0.0.0.0", "224.0.0.1",
		"100.64.0.1", "100.127.255.254", // CGNAT
		"192.0.0.1", "192.0.2.5", "198.18.0.1", "198.51.100.7", "203.0.113.9",
		"240.0.0.1", "255.255.255.255",
		"::1", "fe80::1", "fc00::1", "fec0::1", "2001:db8::1", "64:ff9b::a00:1",
		"::ffff:10.0.0.1", // IPv4-mapped private
		"::ffff:100.64.0.1",
	}
	for _, address := range blocked {
		ip := net.ParseIP(address)
		if ip == nil {
			t.Fatalf("test fixture %q is not an IP", address)
		}
		if !blockedThemeAddress(ip) {
			t.Errorf("%s was allowed, want blocked", address)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"}
	for _, address := range allowed {
		ip := net.ParseIP(address)
		if ip == nil {
			t.Fatalf("test fixture %q is not an IP", address)
		}
		if blockedThemeAddress(ip) {
			t.Errorf("%s was blocked, want allowed", address)
		}
	}

	if !blockedThemeAddress(nil) {
		t.Error("a nil address must be refused")
	}
}
