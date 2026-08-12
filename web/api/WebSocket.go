package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/web/connection"
	"github.com/komari-monitor/komari/web/security"
)

type WebSocketUpgradeOption func(*websocket.Upgrader)

func IsWebSocketUpgrade(c *gin.Context) bool {
	return websocket.IsWebSocketUpgrade(c.Request)
}

func EnableWebSocketCompression(upgrader *websocket.Upgrader) {
	upgrader.EnableCompression = true
}

func RequireSameOriginWebSocket(upgrader *websocket.Upgrader) {
	upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin != "" && security.OriginMatchesRequest(origin, r)
	}
}

// RequireRemoteBrowserOrigin keeps normal same-origin enforcement while
// accepting opaque origins used by some installed mobile web apps. The remote
// endpoint additionally requires an administrator cookie and a single-use,
// high-entropy browser ticket before it can attach to a session.
func RequireRemoteBrowserOrigin(upgrader *websocket.Upgrader) {
	upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		return origin == "" || strings.EqualFold(origin, "null") ||
			security.OriginMatchesRequest(origin, r)
	}
}

func AllowAgentWebSocket(upgrader *websocket.Upgrader) {
	upgrader.CheckOrigin = func(r *http.Request) bool {
		return r.Header.Get("Origin") == ""
	}
}

func UpgradeWebSocket(c *gin.Context, options ...WebSocketUpgradeOption) (*websocket.Conn, error) {
	if !IsWebSocketUpgrade(c) {
		return nil, fmt.Errorf("require websocket upgrade")
	}
	upgrader := websocket.Upgrader{
		CheckOrigin: CheckWebSocketOrigin,
	}
	for _, option := range options {
		option(&upgrader)
	}
	return upgrader.Upgrade(c.Writer, c.Request, nil)
}

// UpgradeSafeConn attaches the process-wide plugin WebSocket hook chain to a
// newly upgraded connection. Legacy callers that need the raw websocket stay
// supported; protocol handlers use this typed wrapper so hooks see every
// JSON-RPC and agent frame.
func UpgradeSafeConn(c *gin.Context, options ...WebSocketUpgradeOption) (*connection.SafeConn, error) {
	unsafeConn, err := UpgradeWebSocket(c, options...)
	if err != nil {
		return nil, err
	}
	interceptor := connection.Interceptor()
	if interceptor == nil {
		return connection.NewSafeConn(unsafeConn), nil
	}
	sc := connection.NewSafeConn(unsafeConn)
	info := &connection.ConnInfo{
		ID:        sc.ID,
		Path:      c.Request.URL.Path,
		RemoteIP:  c.ClientIP(),
		UserAgent: c.Request.UserAgent(),
	}
	if clientUUID, ok := c.Get("client_uuid"); ok {
		if value, ok := clientUUID.(string); ok {
			info.ClientUUID = value
		}
	}
	if deny, reason := interceptor.OnConnect(info); deny {
		_ = unsafeConn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason),
			time.Now().Add(time.Second))
		_ = unsafeConn.Close()
		return nil, errors.New("websocket connection denied by plugin")
	}
	sc.SetInterceptor(info, interceptor)
	return sc, nil
}

func CheckWebSocketOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if strings.EqualFold(os.Getenv("KOMARI_WS_DISABLE_ORIGIN"), "true") {
		return true
	}
	if security.IsAPIKeyRequest(r) {
		return true
	}
	if origin == "" && r.URL.Query().Get("token") != "" {
		return true
	}
	enabled, _ := config.GetAs[bool](config.WsOriginCheckEnabledKey, true)
	if !enabled {
		return true
	}
	if origin == "" {
		return false
	}
	if security.OriginMatchesRequest(origin, r) {
		return true
	}
	allowlist, _ := config.GetAs[string](config.WsAllowedOriginsKey, "")
	return security.OriginInAllowlist(origin, allowlist)
}
