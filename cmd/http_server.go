package cmd

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func newH2CServer(addr string, handler http.Handler) *http.Server {
	const headerTimeout = 10 * time.Second
	const maxUpgradeBody = 64 << 10
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	http2Handler := h2c.NewHandler(handler, &http2.Server{
		MaxConcurrentStreams: 128,
		IdleTimeout:          2 * time.Minute,
		ReadIdleTimeout:      time.Minute,
		PingTimeout:          15 * time.Second,
	})
	return &http.Server{
		Addr:      addr,
		Protocols: protocols,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: 128,
			SendPingTimeout:      time.Minute,
			PingTimeout:          15 * time.Second,
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor != 1 || !httpguts.HeaderValuesContainsToken(r.Header.Values("Upgrade"), "h2c") ||
				!httpguts.HeaderValuesContainsToken(r.Header.Values("Connection"), "HTTP2-Settings") {
				handler.ServeHTTP(w, r)
				return
			}
			// Read the HTTP/1 upgrade body before handing off the connection.
			// Clear its deadline before the HTTP/2 connection takes ownership.
			controller := http.NewResponseController(w)
			if err := controller.SetReadDeadline(time.Now().Add(headerTimeout)); err != nil {
				http.Error(w, "cannot bound upgrade request", http.StatusInternalServerError)
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpgradeBody))
			_ = controller.SetReadDeadline(time.Time{})
			if err != nil {
				var limit *http.MaxBytesError
				if errors.As(err, &limit) {
					http.Error(w, "upgrade request is too large", http.StatusRequestEntityTooLarge)
					return
				}
				w.Header().Set("Connection", "close")
				http.Error(w, "cannot read upgrade request", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			http2Handler.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: headerTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}
