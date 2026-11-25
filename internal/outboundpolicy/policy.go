package outboundpolicy

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/komari-monitor/komari/pkg/config"
	"gorm.io/gorm"
)

func Enabled() (bool, error) {
	enabled, err := config.GetAs[bool](config.SSRFProtectionEnabledKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	return enabled, err
}

func HostHasBlockedAddress(ctx context.Context, host string) bool {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return true // fail closed
	}
	if len(ips) == 0 {
		return true
	}
	for _, address := range ips {
		if BlockedAddress(address.IP) {
			return true
		}
	}
	return false
}

// blockedNonPublicRanges covers addresses that are routable enough for net's
// own predicates to call them global unicast, yet still reach infrastructure
// rather than the public internet. CGNAT in particular is where cloud providers
// put internal endpoints, so it has to be refused explicitly.
var blockedNonPublicRanges = func() []*net.IPNet {
	prefixes := []string{
		"0.0.0.0/8",       // this network
		"100.64.0.0/10",   // RFC 6598 CGNAT
		"192.0.0.0/24",    // RFC 6890 IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // RFC 2544 benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved, includes 255.255.255.255
		"fec0::/10",       // deprecated site-local
		"2001:db8::/32",   // documentation
		"64:ff9b::/96",    // NAT64, can map onto private IPv4
		"64:ff9b:1::/48",  // local-use NAT64
		"2002::/16",       // 6to4 embeds an IPv4 destination
		"2001::/32",       // Teredo embeds IPv4 relay/client addresses
	}
	nets := make([]*net.IPNet, 0, len(prefixes))
	for _, prefix := range prefixes {
		if _, network, err := net.ParseCIDR(prefix); err == nil {
			nets = append(nets, network)
		}
	}
	return nets
}()

func BlockedAddress(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// Judge IPv4-mapped addresses on their underlying IPv4 destination.
		ip = v4
	}
	for _, network := range blockedNonPublicRanges {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// DialPublic validates the address returned at connection time, closing
// DNS rebinding and redirect races left by URL-only validation.
func DialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return DialPublicWith(ctx, network, address, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
}

func DialPublicWith(ctx context.Context, network, address string, lookup func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, candidate := range addresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if BlockedAddress(candidate.IP) {
			continue
		}
		conn, dialErr := dial(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("destination has no permitted public address")
}
