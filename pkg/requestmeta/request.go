// Package requestmeta is the shared trust boundary for HTTP request metadata.
// Transport adapters and raw plugin hooks must not invent their own proxy rules.
package requestmeta

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	TrustedProxyEnv = "KOMARI_TRUSTED_PROXIES"
	PublicOriginEnv = "KOMARI_PUBLIC_ORIGIN"
)

// TrustedProxies defaults to no trusted peers. Private/loopback addresses are
// not identities: only an operator's explicit hosts/CIDRs may forward metadata.
func TrustedProxies(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "none") {
		return nil, nil
	}
	var result []string
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, errors.New("empty trusted proxy entry")
		}
		if ip, err := netip.ParseAddr(entry); err == nil && ip.Zone() == "" {
			result = append(result, ip.Unmap().String())
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil || prefix.Bits() == 0 || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("invalid or unrestricted trusted proxy %q", entry)
		}
		result = append(result, prefix.Masked().String())
	}
	return result, nil
}

func trustedIP(ip netip.Addr, proxies []string) bool {
	for _, entry := range proxies {
		if host, err := netip.ParseAddr(entry); err == nil {
			if host == ip.Unmap() {
				return true
			}
		} else if prefix, err := netip.ParsePrefix(entry); err == nil && prefix.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

func peerAddress(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}

// ClientIP uses the same right-to-left trusted chain as Gin. Invalid settings
// or an untrusted transport peer fail closed to the actual socket address.
func ClientIP(r *http.Request) string {
	peer := peerAddress(r)
	if !peer.IsValid() {
		return ""
	}
	proxies, err := TrustedProxies(os.Getenv(TrustedProxyEnv))
	if err != nil || !trustedIP(peer, proxies) {
		return peer.String()
	}
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP"} {
		value := r.Header.Get(header)
		if value == "" {
			continue
		}
		parts := strings.Split(value, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
			if err != nil || ip.Zone() != "" {
				break
			}
			if i == 0 || !trustedIP(ip, proxies) {
				return ip.Unmap().String()
			}
		}
	}
	return peer.String()
}

// NormalizeOrigin accepts only an HTTP(S) origin, never credentials, a path,
// query, fragment, whitespace, opaque URL or caller-chosen alternate scheme.
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(raw) != raw || u == nil || u.User != nil || u.Opaque != "" ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", errors.New("expected an absolute HTTP(S) origin without a path")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, " \t\r\n/\\%@,;\"'") || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("invalid origin host")
	}
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is6() {
			return "", errors.New("invalid IPv6 origin host")
		}
	} else if strings.Contains(host, ":") {
		return "", errors.New("IPv6 origin host requires brackets")
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid origin port")
		}
		port = strconv.Itoa(n)
	}
	if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}

// ForwardedHTTPS requires an explicit HTTPS assertion from a configured peer.
// It is independent of TLS on the internal connection, for built-in HTTPS UI.
func ForwardedHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	proxies, err := TrustedProxies(os.Getenv(TrustedProxyEnv))
	if err != nil || !trustedIP(peerAddress(r), proxies) {
		return false
	}
	origin, err := forwardedOrigin(r, "http", r.Host)
	return err == nil && strings.HasPrefix(origin, "https://")
}

// PublicOrigin is the operator-pinned authentication origin. Enrollment links
// cannot fall back to Host, forwarding headers or the caller's socket address.
func PublicOrigin() (string, error) {
	raw := strings.TrimSpace(os.Getenv(PublicOriginEnv))
	if strings.HasSuffix(raw, "/") {
		raw = strings.TrimSuffix(raw, "/")
	}
	if raw == "" {
		return "", fmt.Errorf("%s must declare the external authentication origin", PublicOriginEnv)
	}
	return NormalizeOrigin(raw)
}

// ValidateConfig catches invalid configuration at startup. An unset public
// origin leaves ordinary direct access usable, but cannot issue approval links.
func ValidateConfig() error {
	if _, err := TrustedProxies(os.Getenv(TrustedProxyEnv)); err != nil {
		return err
	}
	if strings.TrimSpace(os.Getenv(PublicOriginEnv)) != "" {
		_, err := PublicOrigin()
		return err
	}
	return nil
}

func singleHeader(r *http.Request, name string) (string, error) {
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || values[0] == "" || strings.Contains(values[0], ",") || strings.TrimSpace(values[0]) != values[0] {
		return "", errors.New("ambiguous forwarded origin")
	}
	return values[0], nil
}

func forwardedOrigin(r *http.Request, scheme, host string) (string, error) {
	xhost, err := singleHeader(r, "X-Forwarded-Host")
	if err != nil {
		return "", err
	}
	xscheme, err := singleHeader(r, "X-Forwarded-Proto")
	if err != nil {
		return "", err
	}
	forwarded, err := singleHeader(r, "Forwarded")
	if err != nil {
		return "", err
	}
	fhost, fscheme := "", ""
	for _, parameter := range strings.Split(forwarded, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
		if !ok {
			if parameter != "" {
				return "", errors.New("malformed forwarded origin")
			}
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			value, err = strconv.Unquote(value)
			if err != nil {
				return "", errors.New("malformed quoted forwarded origin")
			}
		}
		switch strings.ToLower(key) {
		case "host":
			if fhost != "" || value == "" {
				return "", errors.New("duplicate or empty forwarded host")
			}
			fhost = value
		case "proto":
			if fscheme != "" || value == "" {
				return "", errors.New("duplicate or empty forwarded scheme")
			}
			fscheme = value
		}
	}
	if xhost != "" && fhost != "" && !strings.EqualFold(xhost, fhost) || xscheme != "" && fscheme != "" && xscheme != fscheme {
		return "", errors.New("conflicting forwarded origin")
	}
	if xhost != "" {
		host = xhost
	} else if fhost != "" {
		host = fhost
	}
	if xscheme != "" {
		scheme = xscheme
	} else if fscheme != "" {
		scheme = fscheme
	}
	return NormalizeOrigin(scheme + "://" + host)
}

// Origin resolves one exact scheme/host/port using the configured proxy trust
// list. If an external origin is pinned, alternate hosts/schemes are rejected.
func Origin(r *http.Request) (string, error) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	origin, err := NormalizeOrigin(scheme + "://" + r.Host)
	if err != nil {
		return "", err
	}
	proxies, err := TrustedProxies(os.Getenv(TrustedProxyEnv))
	if err != nil {
		return "", err
	}
	if trustedIP(peerAddress(r), proxies) {
		origin, err = forwardedOrigin(r, scheme, r.Host)
		if err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(os.Getenv(PublicOriginEnv)) != "" {
		pinned, err := PublicOrigin()
		if err != nil || pinned != origin {
			return "", errors.New("request origin does not match the configured authentication origin")
		}
	}
	return origin, nil
}
