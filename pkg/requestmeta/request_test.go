package requestmeta

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeOriginRejectsNonOrigins(t *testing.T) {
	for _, raw := range []string{"", "null", "ftp://panel.test", "https://user@panel.test", "https://panel.test/", "https://panel.test/path", "https://panel.test?", "https://panel.test#", " https://panel.test", "https://panel.test:0", "https://panel.test:65536", "https://panel.test:", "https://[not-an-ip]", "https://panel.test,evil.test", "https://2001:db8::1"} {
		if value, err := NormalizeOrigin(raw); err == nil {
			t.Errorf("accepted %q as %q", raw, value)
		}
	}
	for raw, want := range map[string]string{"https://Panel.test:443": "https://panel.test", "http://panel.test:80": "http://panel.test", "https://panel.test:8443": "https://panel.test:8443", "https://[::1]:443": "https://[::1]"} {
		if got, err := NormalizeOrigin(raw); err != nil || got != want {
			t.Errorf("normalize %q = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestOriginUsesOnlyExplicitProxyTrust(t *testing.T) {
	t.Setenv(PublicOriginEnv, "")
	for _, trust := range []string{"", "none", "127.0.0.1"} {
		t.Run(trust, func(t *testing.T) {
			t.Setenv(TrustedProxyEnv, trust)
			r := httptest.NewRequest(http.MethodPost, "http://internal.test/api", nil)
			r.RemoteAddr = "172.18.0.2:3000"
			r.Header.Set("X-Forwarded-Host", "evil.test")
			r.Header.Set("X-Forwarded-Proto", "https")
			if got, err := Origin(r); err != nil || got != "http://internal.test" {
				t.Fatalf("untrusted peer changed origin: %q, %v", got, err)
			}
			if ForwardedHTTPS(r) {
				t.Fatal("private address implicitly acquired proxy trust")
			}
		})
	}
	t.Setenv(TrustedProxyEnv, "172.18.0.2")
	t.Setenv(PublicOriginEnv, "https://panel.test")
	for _, headers := range []http.Header{
		{"X-Forwarded-Host": {"panel.test"}, "X-Forwarded-Proto": {"https"}},
		{"Forwarded": {`for=192.0.2.1;host="panel.test";proto=https`}},
	} {
		r := httptest.NewRequest(http.MethodPost, "http://internal.test/api", nil)
		r.RemoteAddr = "172.18.0.2:3000"
		r.Header = headers
		if got, err := Origin(r); err != nil || got != "https://panel.test" || !ForwardedHTTPS(r) {
			t.Fatalf("explicit proxy lost valid origin: %q, %v", got, err)
		}
	}
}

func TestOriginRejectsAmbiguousOrUnpinnedForwarding(t *testing.T) {
	t.Setenv(TrustedProxyEnv, "127.0.0.1")
	t.Setenv(PublicOriginEnv, "https://panel.test")
	for _, headers := range []http.Header{
		{"X-Forwarded-Host": {"evil.test"}, "X-Forwarded-Proto": {"https"}},
		{"X-Forwarded-Host": {"panel.test"}, "X-Forwarded-Proto": {"http"}},
		{"X-Forwarded-Host": {"panel.test", "evil.test"}, "X-Forwarded-Proto": {"https"}},
		{"X-Forwarded-Host": {"evil.test, panel.test"}, "X-Forwarded-Proto": {"https"}},
		{"X-Forwarded-Host": {"panel.test"}, "X-Forwarded-Proto": {"http, https"}},
		{"X-Forwarded-Host": {"panel.test"}, "X-Forwarded-Proto": {"https"}, "Forwarded": {"host=evil.test;proto=https"}},
		{"Forwarded": {"host=panel.test;host=evil.test;proto=https"}},
	} {
		r := httptest.NewRequest(http.MethodPost, "http://internal.test/api", nil)
		r.RemoteAddr = "127.0.0.1:3000"
		r.Header = headers
		if got, err := Origin(r); err == nil {
			t.Errorf("accepted ambiguous/unpinned origin %q with %v", got, headers)
		}
	}
}

func TestClientIPStopsAtUntrustedHop(t *testing.T) {
	for _, tc := range []struct{ trust, peer, forwarded, want string }{
		{"", "127.0.0.1:3000", "192.0.2.1", "127.0.0.1"},
		{"none", "172.18.0.2:3000", "192.0.2.1", "172.18.0.2"},
		{"127.0.0.1", "127.0.0.1:3000", "10.0.0.1, 192.0.2.1", "192.0.2.1"},
		{"127.0.0.1,192.0.2.0/24", "127.0.0.1:3000", "198.51.100.1, 192.0.2.1", "198.51.100.1"},
		{"127.0.0.1", "203.0.113.1:3000", "192.0.2.1", "203.0.113.1"},
		{"not-a-proxy", "127.0.0.1:3000", "192.0.2.1", "127.0.0.1"},
	} {
		t.Setenv(TrustedProxyEnv, tc.trust)
		r := httptest.NewRequest(http.MethodGet, "http://panel.test", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.forwarded)
		if got := ClientIP(r); got != tc.want {
			t.Errorf("trust=%q peer=%q: got %q, want %q", tc.trust, tc.peer, got, tc.want)
		}
	}
}

func TestPublicOriginRequiresExplicitConfiguration(t *testing.T) {
	t.Setenv(TrustedProxyEnv, "none")
	for _, raw := range []string{"", "https://panel.test/bind", "https://user@panel.test"} {
		t.Setenv(PublicOriginEnv, raw)
		if got, err := PublicOrigin(); err == nil {
			t.Errorf("accepted invalid public origin %q: %q", raw, got)
		}
	}
	t.Setenv(PublicOriginEnv, "https://panel.test/")
	if got, err := PublicOrigin(); err != nil || got != "https://panel.test" {
		t.Fatalf("valid configured origin rejected: %q, %v", got, err)
	}
	for _, raw := range []string{"0.0.0.0/0", "::/0", " , ", "127.0.0.1,", "not-a-proxy"} {
		if _, err := TrustedProxies(raw); err == nil {
			t.Errorf("accepted malformed or trust-all proxy list %q", raw)
		}
	}
}
