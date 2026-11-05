package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/komari-monitor/komari/pkg/config"
	"gorm.io/gorm"
)

var errMarketDownloadTooLarge = errors.New("market download exceeds size limit")

// Separate pools prevent a connection opened while protection was disabled
// from being reused after protection is enabled. Neither transport uses proxies.
var protectedMarketTransport = marketTransport(true)
var unrestrictedMarketTransport = marketTransport(false)

func marketTransport(protected bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if protected {
		transport.DialContext = dialPublicAddress
	}
	return transport
}

func ssrfProtectionEnabled() (bool, error) {
	enabled, err := config.GetAs[bool](config.SSRFProtectionEnabledKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	return enabled, err
}

func validateMarketDownloadURL(ctx context.Context, rawURL string, protected bool) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("only HTTP and HTTPS URLs without embedded credentials are allowed")
	}
	if protected && isPrivateIPContext(ctx, parsed.Hostname()) {
		return errors.New("requests to private or non-public addresses are not allowed")
	}
	return nil
}

func validateMarketRedirect(req *http.Request, via []*http.Request, protected bool) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return validateMarketDownloadURL(req.Context(), req.URL.String(), protected)
}

func downloadMarketURL(rawURL string, maxSize int64, timeout time.Duration) ([]byte, error) {
	protected, err := ssrfProtectionEnabled()
	if err != nil {
		return nil, fmt.Errorf("unable to read SSRF protection setting: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := validateMarketDownloadURL(ctx, rawURL, protected); err != nil {
		return nil, err
	}
	transport := unrestrictedMarketTransport
	if protected {
		transport = protectedMarketTransport
	}
	// Snapshot the setting for this download, including all redirect hops.
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return validateMarketRedirect(req, via, protected)
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", response.StatusCode)
	}
	if response.ContentLength > maxSize {
		return nil, fmt.Errorf("%w: %d bytes", errMarketDownloadTooLarge, maxSize)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("%w: %d bytes", errMarketDownloadTooLarge, maxSize)
	}
	if len(data) == 0 {
		return nil, errors.New("empty response")
	}
	return data, nil
}
