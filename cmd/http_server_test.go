package cmd

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestPanelListenerSupportsHTTP1AndCleartextHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	server.Config = newH2CServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write(payload)
	}))
	server.Start()
	t.Cleanup(server.Close)
	priorKnowledge := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(priorKnowledge.CloseIdleConnections)
	body := strings.Repeat("x", 128<<10)
	for _, tc := range []struct {
		client *http.Client
		major  int
	}{
		{server.Client(), 1},
		{&http.Client{Transport: priorKnowledge, Timeout: 5 * time.Second}, 2},
	} {
		response, err := tc.client.Post(server.URL, "application/octet-stream", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || response.ProtoMajor != tc.major || string(payload) != body {
			t.Fatalf("HTTP/%d response: %s over %s, received %d bytes, error %v", tc.major, response.Status, response.Proto, len(payload), err)
		}
	}
}

func TestPanelListenerLimitsH2CUpgradeBody(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	server.Config = newH2CServer("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Start()
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(strings.Repeat("x", (64<<10)+1)))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	r.Header.Set("Upgrade", "h2c")
	r.Header.Set("HTTP2-Settings", "AAMAAABk")
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("upgrade returned %s", response.Status)
	}
}
