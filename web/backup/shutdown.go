package backup

import (
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	logger "github.com/komari-monitor/komari/utils/log"
	"github.com/komari-monitor/komari/web/security"
)

var (
	restartMu      sync.Mutex
	restartHandler func(reason string)
	restartPending atomic.Bool
)

// RegisterRestartHandler registers the process-wide graceful shutdown callback
// triggered after a staged backup is accepted. Passing nil clears it.
func RegisterRestartHandler(fn func(reason string)) {
	restartMu.Lock()
	restartHandler = fn
	restartMu.Unlock()
}

// MarkRestartPending marks the server as draining for a staged backup restore.
func MarkRestartPending() {
	restartPending.Store(true)
}

// IsRestartPending reports whether a staged restore restart has been scheduled.
func IsRestartPending() bool {
	return restartPending.Load()
}

// RequestGracefulRestart triggers the registered graceful shutdown callback.
// Outside a supervised server lifecycle (e.g. standalone handler tests), it
// marks restart pending and logs rather than calling os.Exit(0).
func RequestGracefulRestart(reason string) {
	MarkRestartPending()
	restartMu.Lock()
	fn := restartHandler
	restartMu.Unlock()
	if fn == nil {
		logger.Infof("backup", "[restore] graceful restart requested (%s); no supervisor handler registered (manual restart required outside containers)", reason)
		return
	}
	fn(reason)
}

// ResetRestartStateForTest clears restart state for isolated tests.
func ResetRestartStateForTest() {
	restartPending.Store(false)
	RegisterRestartHandler(nil)
}

// RejectMutationsWhileRestarting fails closed on mutating requests once a
// backup archive has been staged and graceful shutdown is underway.
func RejectMutationsWhileRestarting() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restartPending.Load() {
			c.Next()
			return
		}
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		c.Header("Retry-After", "5")
		security.AbortRequest(c, http.StatusServiceUnavailable, "backup restore staged; server is shutting down to apply backup")
	}
}
