package geoip_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/komari-monitor/komari/utils/geoip"
)

// 测试GeoIP数据库的初始化和更新功能
func TestMmdb(t *testing.T) {
	geoip.CurrentProvider, _ = geoip.NewMaxMindGeoIPService()
	testIpAddr(t)
}
func TestIpApi(t *testing.T) {
	provider, err := geoip.NewIPAPIService()
	if err != nil {
		t.Fatal(err)
	}
	provider.Client = providerFixture(t, "ip-api.com", `{"status":"success","countryCode":"US","country":"United States"}`)
	geoip.CurrentProvider = provider
	testIpAddr(t)
}

func TestGeojs(t *testing.T) {
	provider, err := geoip.NewGeoJSService()
	if err != nil {
		t.Fatal(err)
	}
	provider.Client = providerFixture(t, "get.geojs.io", `{"country_code":"US","country":"United States"}`)
	geoip.CurrentProvider = provider
	testIpAddr(t)
}

func TestIpInfo(t *testing.T) {
	provider, err := geoip.NewIPInfoService()
	if err != nil {
		t.Fatal(err)
	}
	provider.Client = providerFixture(t, "ipinfo.io", `{"country":"US"}`)
	geoip.CurrentProvider = provider
	testIpAddr(t)
}
func testIpAddr(t *testing.T) {
	// IPv4
	ipaddr := "8.8.8.8"
	ip := net.ParseIP(ipaddr)
	record, err := geoip.GetGeoInfo(ip)
	if err != nil {
		t.Fatalf("Failed to get GeoIP info for IP %s: %v", ipaddr, err)
	}

	if record != nil {
		if record.ISOCode == "" && record.Name == "" {
			t.Errorf("Country information is missing for IP %s", ipaddr)
		}
	} else {
		t.Fatalf("GeoIP record is nil for IP %s", ipaddr)
	}

	t.Logf("IPv4:[%s]%s - %s", ipaddr, record.ISOCode, record.Name)

	// IPv6
	ipaddr = "2001:4860:4860::8888"
	ip = net.ParseIP(ipaddr)
	record, err = geoip.GetGeoInfo(ip)
	if err != nil {
		t.Fatalf("Failed to get GeoIP info for IPv6 %s: %v", ipaddr, err)
	}
	if record != nil {
		if record.ISOCode == "" && record.Name == "" {
			t.Errorf("Country information is missing for IPv6 %s", ipaddr)
		}
	} else {
		t.Fatalf("GeoIP record is nil for IPv6 %s", ipaddr)
	}
	t.Logf("IPv6:[%s]%s - %s", ipaddr, record.ISOCode, record.Name)
}

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise each real provider's URL construction and JSON decoder without
// spending a shared public-IP quota on every CI package run.
func providerFixture(t *testing.T, host, body string) *http.Client {
	t.Helper()
	return &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != host || (!strings.Contains(r.URL.Path, "8.8.8.8") && !strings.Contains(r.URL.Path, "2001:4860:4860::8888")) {
			t.Fatalf("unexpected GeoIP request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
}

func TestHTTPProvidersRejectRateLimitAndMalformedResponse(t *testing.T) {
	for _, status := range []int{200, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader("<html>upstream error</html>")), Request: r}, nil
			})}
			for _, provider := range []geoip.GeoIPService{&geoip.IPInfoService{Client: client}, &geoip.IPAPIService{Client: client}, &geoip.GeoJSService{Client: client}} {
				if info, err := provider.GetGeoInfo(net.ParseIP("8.8.8.8")); err == nil || info != nil {
					t.Fatalf("%s accepted invalid upstream: %+v, %v", provider.Name(), info, err)
				}
			}
		})
	}
}

func TestUnicodeEmoji(t *testing.T) {
	ISOCode := "CN"
	emoji := geoip.GetRegionUnicodeEmoji(ISOCode)
	if emoji != "🇨🇳" {
		t.Errorf("Expected emoji for %s, got %s", ISOCode, emoji)
	}
	t.Logf("Emoji for %s: %s", ISOCode, emoji)
}
