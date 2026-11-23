package net

import "testing"

func TestRejectPrivateAddressBlocksPrivateAndNAT64Ranges(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80",
		"0.0.0.1:80",
		"100.64.0.1:80",
		"10.0.0.1:80",
		"172.16.0.1:80",
		"192.168.1.1:80",
		"169.254.169.254:80",
		"[::1]:80",
		"[64:ff9b::127.0.0.1]:80",
		"[64:ff9b:1::10.0.0.1]:80",
		"[::ffff:0.0.0.1]:80",
		"[::ffff:127.0.0.1]:80",
	}
	for _, addr := range blocked {
		if err := RejectPrivateAddress("tcp", addr, nil); err == nil {
			t.Fatalf("RejectPrivateAddress(%q) unexpectedly allowed private/special address", addr)
		}
	}

	allowed := []string{
		"1.1.1.1:443",
		"[2606:4700:4700::1111]:443",
	}
	for _, addr := range allowed {
		if err := RejectPrivateAddress("tcp", addr, nil); err != nil {
			t.Fatalf("RejectPrivateAddress(%q) unexpectedly rejected public address: %v", addr, err)
		}
	}
}
