// Package requestmeta resolves transport metadata once, using one proxy policy.
// Resolving a destination is separate from authorizing an Origin or credential.
package requestmeta

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	TrustedProxyEnv = "KOMARI_TRUSTED_PROXIES"
	PublicOriginEnv = "KOMARI_PUBLIC_ORIGIN"
	LoopbackProxies = "127.0.0.1,::1"
)

// Policy is compiled at listener startup. The default supports same-host
// proxies; private networks require explicit configuration. "none" opts out.
type Policy struct {
	proxies      []netip.Prefix
	addresses    []string
	publicOrigin string
}

func TrustedProxies(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "none") {
		return nil, nil
	}
	if value == "" {
		value = LoopbackProxies
	}
	var result []string
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
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

func NewPolicy(proxies, publicOrigin string) (*Policy, error) {
	addresses, err := TrustedProxies(proxies)
	if err != nil {
		return nil, err
	}
	p := &Policy{addresses: addresses}
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address)
		if err != nil {
			ip, _ := netip.ParseAddr(address)
			prefix = netip.PrefixFrom(ip, ip.BitLen())
		}
		p.proxies = append(p.proxies, prefix)
	}
	if raw := strings.TrimSuffix(strings.TrimSpace(publicOrigin), "/"); raw != "" {
		p.publicOrigin, err = NormalizeOrigin(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", PublicOriginEnv, err)
		}
	}
	return p, nil
}

func (p *Policy) ProxyAddresses() []string { return append([]string(nil), p.addresses...) }

func (p *Policy) trusted(ip netip.Addr) bool {
	for _, prefix := range p.proxies {
		if prefix.Contains(ip.Unmap()) {
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

func (p *Policy) clientIP(r *http.Request) string {
	peer := peerAddress(r)
	if !peer.IsValid() {
		return ""
	}
	if !p.trusted(peer) {
		return peer.String()
	}
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP"} {
		parts := strings.Split(strings.Join(r.Header.Values(header), ","), ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
			if err != nil || ip.Zone() != "" {
				break
			}
			if i == 0 || !p.trusted(ip) {
				return ip.Unmap().String()
			}
		}
	}
	return peer.String()
}

func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || strings.TrimSpace(raw) != raw || u == nil || u.User != nil || u.Opaque != "" ||
		u.Host == "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", errors.New("expected an absolute HTTP(S) origin without a path")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("expected HTTP(S) origin")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, " \t\r\n/\\%@,;\"'") || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("invalid origin host")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", errors.New("origin cannot contain an IPv6 zone")
		}
		host = ip.String()
	} else if strings.ContainsAny(u.Host, "[]") || strings.Contains(host, ":") {
		return "", errors.New("invalid IPv6 origin host")
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

// The closest trusted proxy's appended value wins. Earlier entries can be
// client supplied. The ingress should overwrite forwarding fields at the edge.
func lastHeader(r *http.Request, name string) (string, bool, error) {
	rawValues := r.Header.Values(name)
	if len(rawValues) == 0 {
		return "", false, nil
	}
	values := strings.Split(strings.Join(rawValues, ","), ",")
	last := strings.TrimSpace(values[len(values)-1])
	if last == "" {
		return "", true, fmt.Errorf("empty %s header", name)
	}
	return last, true, nil
}

func forwardedElement(r *http.Request) (string, error) {
	raw := strings.Join(r.Header.Values("Forwarded"), ",")
	quoted, escaped, start := false, false, 0
	for i, c := range raw {
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
		}
		if c == ',' && !quoted {
			start = i + 1
		}
	}
	if quoted || escaped {
		return "", errors.New("malformed Forwarded header")
	}
	return strings.TrimSpace(raw[start:]), nil
}

func forwardedOrigin(r *http.Request, scheme, host string) (string, error) {
	xhost, hasHost, err := lastHeader(r, "X-Forwarded-Host")
	if err != nil {
		return "", err
	}
	xscheme, hasScheme, err := lastHeader(r, "X-Forwarded-Proto")
	if err != nil {
		return "", err
	}
	// Do not combine a retained RFC Forwarded field with the X-Forwarded
	// fields supplied by the configured local ingress. Prefer that one family.
	if hasHost || hasScheme {
		if hasHost {
			host = xhost
		}
		if hasScheme {
			scheme = xscheme
		}
	} else {
		element, err := forwardedElement(r)
		if err != nil {
			return "", err
		}
		seen := make(map[string]bool)
		for _, parameter := range strings.Split(element, ";") {
			if strings.TrimSpace(parameter) == "" {
				continue
			}
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			key = strings.ToLower(key)
			if !ok || seen[key] {
				return "", errors.New("malformed Forwarded parameter")
			}
			seen[key] = true
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, "\"") {
				value, err = strconv.Unquote(value)
				if err != nil {
					return "", err
				}
			}
			switch key {
			case "host":
				host = value
			case "proto":
				scheme = value
			}
		}
	}
	return NormalizeOrigin(strings.ToLower(scheme) + "://" + host)
}

type metadataKey struct{}
type metadata struct {
	origin         string
	err            error
	clientIP       string
	forwardedHTTPS bool
	publicOrigin   string
}

// BindRequest does no authorization: health/install/upgrade listeners keep
// working through a local transport even with a canonical public URL set.
func (p *Policy) BindRequest(r *http.Request) *http.Request {
	m := p.resolve(r)
	return r.WithContext(context.WithValue(r.Context(), metadataKey{}, m))
}

// EnsureBound binds environment-derived request metadata to r when no listener
// policy has bound it yet, preserving any already-bound listener metadata.
func EnsureBound(r *http.Request) (*http.Request, error) {
	if r == nil {
		return nil, errors.New("request is required")
	}
	if m, ok := r.Context().Value(metadataKey{}).(*metadata); ok {
		return r, m.err
	}
	p, err := FromEnvironment()
	if err != nil {
		return nil, err
	}
	bound := p.BindRequest(r)
	m := bound.Context().Value(metadataKey{}).(*metadata)
	return bound, m.err
}

func (p *Policy) resolve(r *http.Request) *metadata {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	origin, err := NormalizeOrigin(scheme + "://" + r.Host)
	forwarded := p.trusted(peerAddress(r))
	if forwarded {
		origin, err = forwardedOrigin(r, scheme, r.Host)
	}
	return &metadata{origin: origin, err: err, clientIP: p.clientIP(r), forwardedHTTPS: forwarded && err == nil && strings.HasPrefix(origin, "https://"), publicOrigin: p.publicOrigin}
}

var environmentPolicy struct {
	sync.Mutex
	proxies, public string
	policy          *Policy
	err             error
}

// Listeners bind at startup; this cached fallback supports standalone raw
// handlers without parsing the proxy CIDRs again for every metadata accessor.
func FromEnvironment() (*Policy, error) {
	proxies, public := os.Getenv(TrustedProxyEnv), os.Getenv(PublicOriginEnv)
	environmentPolicy.Lock()
	defer environmentPolicy.Unlock()
	if environmentPolicy.policy == nil || proxies != environmentPolicy.proxies || public != environmentPolicy.public {
		environmentPolicy.policy, environmentPolicy.err = NewPolicy(proxies, public)
		environmentPolicy.proxies, environmentPolicy.public = proxies, public
	}
	return environmentPolicy.policy, environmentPolicy.err
}

func forRequest(r *http.Request) *metadata {
	if m, ok := r.Context().Value(metadataKey{}).(*metadata); ok {
		return m
	}
	p, err := FromEnvironment()
	if err != nil {
		return &metadata{err: err}
	}
	return p.resolve(r)
}

func ClientIP(r *http.Request) string        { return forRequest(r).clientIP }
func Origin(r *http.Request) (string, error) { m := forRequest(r); return m.origin, m.err }
func ForwardedHTTPS(r *http.Request) bool    { return r != nil && forRequest(r).forwardedHTTPS }

// PublicURL generates links; it does not accept or reject incoming hosts.
// Configuration wins; existing script-domain settings and direct access work.
func PublicURL(ctx context.Context, configured string) (string, error) {
	m, _ := ctx.Value(metadataKey{}).(*metadata)
	if m == nil {
		p, err := FromEnvironment()
		if err != nil {
			return "", err
		}
		m = &metadata{publicOrigin: p.publicOrigin}
	}
	if m.publicOrigin != "" {
		return m.publicOrigin, nil
	}
	if raw := strings.TrimRight(strings.TrimSpace(configured), "/"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", errors.New("invalid public URL")
		}
		origin, err := NormalizeOrigin(u.Scheme + "://" + u.Host)
		if err != nil {
			return "", err
		}
		return origin + u.EscapedPath(), nil
	}
	if m.err != nil {
		return "", m.err
	}
	if m.origin == "" {
		return "", errors.New("public URL requires an HTTP request or configured panel URL")
	}
	return m.origin, nil
}
