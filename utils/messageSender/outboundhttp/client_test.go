package outboundhttp

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadNotificationResponseBoundsTheEntireBody(t *testing.T) {
	body := strings.Repeat("x", maxNotificationResponseSize)
	got, err := ReadNotificationResponse(strings.NewReader(body))
	if err != nil || string(got) != body {
		t.Fatalf("response at the limit: bytes=%d err=%v", len(got), err)
	}
	got, err = ReadNotificationResponse(strings.NewReader(body + "x"))
	if err == nil || got != nil {
		t.Fatalf("oversize response: bytes=%d err=%v", len(got), err)
	}
}

func TestIPv6FirstDialContextPrefersIPv6AndFallsBackToIPv4(t *testing.T) {
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("192.0.2.1")},
			{IP: net.ParseIP("2001:db8::1")},
		}, nil
	}
	var attempted []string
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		attempted = append(attempted, address)
		return nil, errors.New("unreachable")
	}
	_, err := ipv6FirstDialContext(lookup, dial)(context.Background(), "tcp", "notify.example:443")
	if err == nil {
		t.Fatal("dial succeeded unexpectedly")
	}
	want := []string{"[2001:db8::1]:443", "192.0.2.1:443"}
	if !reflect.DeepEqual(attempted, want) {
		t.Fatalf("dial order = %v, want %v", attempted, want)
	}
}

func TestIPv6FirstDialContextRacesIPv4AfterFallbackDelay(t *testing.T) {
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("2001:db8::1")},
			{IP: net.ParseIP("192.0.2.1")},
		}, nil
	}
	attempted := make(chan string, 2)
	dial := func(ctx context.Context, _ string, address string) (net.Conn, error) {
		attempted <- address
		if address == "192.0.2.1:443" {
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	started := time.Now()
	conn, err := ipv6FirstDialContextWithDelay(lookup, dial, 10*time.Millisecond)(
		context.Background(),
		"tcp",
		"notify.example:443",
	)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("IPv4 fallback took too long: %s", elapsed)
	}

	first, second := <-attempted, <-attempted
	if first != "[2001:db8::1]:443" || second != "192.0.2.1:443" {
		t.Fatalf("dial attempts = %q, %q", first, second)
	}
}

func TestIPv6FirstDialContextSupportsIPv6OnlyEndpoint(t *testing.T) {
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("2001:db8::1")}}, nil
	}
	dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		if address != "[2001:db8::1]:443" {
			t.Fatalf("unexpected address: %s", address)
		}
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}

	conn, err := ipv6FirstDialContext(lookup, dial)(context.Background(), "tcp", "notify.example:443")
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	_ = conn.Close()
}
