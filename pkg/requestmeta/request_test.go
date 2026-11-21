package requestmeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeOrigin(t *testing.T) {
	for _, raw := range []string{"", "null", "ftp://panel.test", "https://user@panel.test", "https://panel.test/", "https://panel.test/path", "https://panel.test?", "https://panel.test#", " https://panel.test", "https://panel.test:0", "https://panel.test:65536", "https://panel.test:", "https://[not-an-ip]", "https://panel.test,evil.test", "https://2001:db8::1"} {
		if value, err := NormalizeOrigin(raw); err == nil {
			t.Errorf("accepted %q as %q", raw, value)
		}
	}
	for raw, want := range map[string]string{"HTTPS://Panel.test:443": "https://panel.test", "http://panel.test:80": "http://panel.test", "https://panel.test:8443": "https://panel.test:8443", "https://[0:0:0:0:0:0:0:1]:8443": "https://[::1]:8443", "https://[::ffff:127.0.0.1]": "https://[::ffff:127.0.0.1]"} {
		if got, err := NormalizeOrigin(raw); err != nil || got != want {
			t.Errorf("%q -> %q (%v), want %q", raw, got, err, want)
		}
	}
}

func TestNormalizeAllowlist(t *testing.T) {
	for _, raw := range []string{"*", "panel.test", "https://panel.test/*", "https://panel.test/app"} {
		if _, err := NormalizeAllowlist(raw, false); err == nil {
			t.Errorf("strict save accepted %q", raw)
		}
	}
	if got, err := NormalizeAllowlist("panel.test:8443, *, https://app.test/", true); err != nil || got != "http://panel.test:8443,https://panel.test:8443,*,https://app.test" {
		t.Fatalf("legacy allowlist = %q (%v)", got, err)
	}
	if got, err := NormalizeAllowlist("HTTPS://Console.test:443\nhttp://127.0.0.1:25774", false); err != nil || got != "https://console.test,http://127.0.0.1:25774" {
		t.Fatalf("strict allowlist = %q (%v)", got, err)
	}
}

func TestTrustedProxiesDefaultsAndValidation(t *testing.T) {
	defaults, err := TrustedProxies("")
	if err != nil || len(defaults) != 2 || defaults[0] != "127.0.0.1" || defaults[1] != "::1" {
		t.Fatalf("default proxies = %v (%v)", defaults, err)
	}
	if none, err := TrustedProxies("none"); err != nil || none != nil {
		t.Fatalf("none = %v (%v)", none, err)
	}
	for _, invalid := range []string{" , ", "0.0.0.0/0", "::/0", "10.0.0.1/33", "::ffff:127.0.0.0/104", "fe80::1%eth0", "proxy.local"} {
		if _, err := TrustedProxies(invalid); err == nil {
			t.Errorf("accepted unrestricted or invalid proxy %q", invalid)
		}
	}
}

func TestPolicyUsesSameProxySetForIPAndOrigin(t *testing.T) {
	policy, err := NewPolicy("127.0.0.1,10.0.0.0/24", "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:25774/api/test", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 10.0.0.5")
	req.Header.Add("X-Forwarded-Host", "spoof.test")
	req.Header.Add("X-Forwarded-Host", "panel.test:443")
	req.Header.Add("X-Forwarded-Proto", "http, https")
	req.Header.Set("Forwarded", "host=ignored.test;proto=http")
	bound := policy.BindRequest(req)
	if got := ClientIP(bound); got != "198.51.100.1" {
		t.Fatalf("client IP = %q", got)
	}
	if got, err := Origin(bound); err != nil || got != "https://panel.test" {
		t.Fatalf("origin = %q (%v)", got, err)
	}
	if !ForwardedHTTPS(bound) {
		t.Fatal("missing forwarded HTTPS flag")
	}

	req.Header = http.Header{"Forwarded": {`for="198.51.100.2", host="one.test";proto=http, for=10.0.0.5;host="two.test:8443";proto=https`}}
	bound = policy.BindRequest(req)
	if got, err := Origin(bound); err != nil || got != "https://two.test:8443" {
		t.Fatalf("Forwarded origin = %q (%v)", got, err)
	}

	req.RemoteAddr = "203.0.113.9:40000"
	bound = policy.BindRequest(req)
	if got := ClientIP(bound); got != "203.0.113.9" {
		t.Fatalf("untrusted peer IP = %q", got)
	}
	if got, err := Origin(bound); err != nil || got != "http://127.0.0.1:25774" {
		t.Fatalf("untrusted peer chose origin %q (%v)", got, err)
	}
	if ForwardedHTTPS(bound) {
		t.Fatal("untrusted peer forged HTTPS")
	}
}

func TestForwardedParsingRejectsAmbiguity(t *testing.T) {
	policy, err := NewPolicy("127.0.0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []http.Header{
		{"X-Forwarded-Host": {"panel.test, "}},
		{"X-Forwarded-Host": {"a.test b.test"}, "X-Forwarded-Proto": {"https"}},
		{"X-Forwarded-Host": {"bad host"}, "X-Forwarded-Proto": {"https"}},
		{"X-Forwarded-Host": {"panel.test/path"}, "X-Forwarded-Proto": {"https"}},
		{"X-Forwarded-Proto": {"https, "}},
		{"Forwarded": {`host=panel.test;host=evil.test`}},
		{"Forwarded": {`host="unterminated`}},
		{"X-Forwarded-Host": {"panel.test"}, "X-Forwarded-Proto": {"ftp"}},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
		req.RemoteAddr = "127.0.0.1:1"
		req.Header = header
		if origin, err := Origin(policy.BindRequest(req)); err == nil {
			t.Errorf("accepted ambiguous header %v as %q", header, origin)
		}
	}
}

func TestPublicURLPreferenceAndBinding(t *testing.T) {
	if _, err := PublicURL(context.Background(), ""); err == nil {
		t.Fatal("empty context invented a URL")
	}
	policy, err := NewPolicy("none", "")
	if err != nil {
		t.Fatal(err)
	}
	req := policy.BindRequest(httptest.NewRequest(http.MethodGet, "https://panel.test/api/enroll", nil))
	if got, err := PublicURL(req.Context(), ""); err != nil || got != "https://panel.test" {
		t.Fatalf("direct PublicURL = %q (%v)", got, err)
	}
	if got, err := PublicURL(req.Context(), "https://Script.test:443/prefix/"); err != nil || got != "https://script.test/prefix" {
		t.Fatalf("configured PublicURL = %q (%v)", got, err)
	}
	pinned, err := NewPolicy("none", "https://Canonical.test:443/")
	if err != nil {
		t.Fatal(err)
	}
	req = pinned.BindRequest(httptest.NewRequest(http.MethodGet, "http://127.0.0.1:25774/", nil))
	if got, err := PublicURL(req.Context(), "https://script.test/"); err != nil || got != "https://canonical.test" {
		t.Fatalf("env PublicURL = %q (%v)", got, err)
	}
}
