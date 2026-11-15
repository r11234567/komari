package requestscheme

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsHTTPS(t *testing.T) {
	t.Setenv("KOMARI_TRUSTED_PROXIES", "127.0.0.1,172.18.0.3,fd00::2")
	tests := []struct {
		name       string
		url        string
		remoteAddr string
		headers    http.Header
		want       bool
	}{
		{name: "direct TLS", url: "https://example.test/admin", remoteAddr: "203.0.113.10:42000", want: true},
		{name: "trusted local proxy", url: "http://example.test/admin", remoteAddr: "127.0.0.1:42000", headers: http.Header{"X-Forwarded-Proto": []string{"https"}}, want: true},
		{name: "ambiguous chain rejected", url: "http://example.test/admin", remoteAddr: "172.18.0.3:42000", headers: http.Header{"X-Forwarded-Proto": []string{"http, https"}}, want: false},
		{name: "explicit docker proxy", url: "http://example.test/admin", remoteAddr: "172.18.0.3:42000", headers: http.Header{"X-Forwarded-Proto": []string{"https"}}, want: true},
		{name: "unlisted private peer", url: "http://example.test/admin", remoteAddr: "172.18.0.4:42000", headers: http.Header{"X-Forwarded-Proto": []string{"https"}}, want: false},
		{name: "trusted IPv6 proxy", url: "http://example.test/admin", remoteAddr: "[fd00::2]:42000", headers: http.Header{"Forwarded": []string{`for=203.0.113.10; proto="https"`}}, want: true},
		{name: "public client cannot spoof proxy header", url: "http://example.test/admin", remoteAddr: "203.0.113.10:42000", headers: http.Header{"X-Forwarded-Proto": []string{"https"}}, want: false},
		{name: "plain HTTP", url: "http://example.test/admin", remoteAddr: "203.0.113.10:42000", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tt.url, nil)
			request.RemoteAddr = tt.remoteAddr
			request.Header = tt.headers
			if got := IsHTTPS(request); got != tt.want {
				t.Fatalf("IsHTTPS() = %t, want %t", got, tt.want)
			}
		})
	}
}
