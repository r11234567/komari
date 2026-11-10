package admin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestThemeDialRevalidatesDNSAndUsesOnlyNumericPublicAddresses(t *testing.T) {
	lookups, dials := 0, 0
	lookup := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if host != "rebind.invalid" {
			t.Fatalf("unexpected host %s", host)
		}
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("::ffff:169.254.169.254")}, {IP: net.ParseIP("fd00::1")}}, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if network != "tcp" || address != "1.1.1.1:443" {
			t.Fatalf("unsafe dial %s %s", network, address)
		}
		a, b := net.Pipe()
		_ = b.Close()
		return a, nil
	}
	conn, err := dialPublicAddressWith(context.Background(), "tcp", "rebind.invalid:443", lookup, dial)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if _, err = dialPublicAddressWith(context.Background(), "tcp", "rebind.invalid:443", lookup, dial); err == nil {
		t.Fatal("rebound private DNS destination accepted")
	}
	if lookups != 2 || dials != 1 {
		t.Fatalf("lookups=%d dials=%d", lookups, dials)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = dialPublicAddressWith(ctx, "tcp", "rebind.invalid:443", lookup, dial); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
	if dials != 1 {
		t.Fatal("cancelled request dialled")
	}
}

type themeRedirectTransport func(*http.Request) (*http.Response, error)

func (f themeRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestThemeRedirectBlocksPrivateIPv4IPv6AndUnsupportedSchemes(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/secret", "http://169.254.169.254/metadata", "https://[::1]/", "https://[::ffff:10.0.0.1]/", "https://[fd00::1]/", "ftp://1.1.1.1/file"} {
		t.Run(target, func(t *testing.T) {
			requests := 0
			client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return validateMarketRedirect(req, via, true) }, Transport: themeRedirectTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			response, err := client.Get("https://public.invalid/theme.zip")
			if response != nil {
				_ = response.Body.Close()
			}
			if err == nil || requests != 1 {
				t.Fatalf("redirect escaped validation: requests=%d err=%v", requests, err)
			}
		})
	}
	u, _ := url.Parse("https://1.1.1.1/")
	if err := validateMarketRedirect(&http.Request{URL: u}, make([]*http.Request, 10), true); err == nil {
		t.Fatal("redirect chain unbounded")
	}
}

// blockedThemeAddress is the last line of defence for market/theme downloads:
// it judges the address actually being dialled, after DNS has resolved. Ranges
// that Go's own predicates call global unicast still reach infrastructure, and
// CGNAT is where cloud providers put internal endpoints.
func TestBlockedThemeAddressRejectsNonPublicRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", // cloud metadata
		"0.0.0.0", "224.0.0.1",
		"0.1.2.3", "64:ff9b:1::a00:1", "2002:0a00:1::", "2001::1",
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
