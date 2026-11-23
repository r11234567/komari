package security

import (
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/config"
)

// CorsController 保存 CORS 中间件的可热更新状态。
//
// 相比过去把 config.Subscribe 直接埋在中间件闭包里，这里把「当前配置」抽成显式对象：
//   - 需要热更新的状态集中在 controller 上，由外部（启动生命周期里的 reload 管理器）
//     统一调用 Update，避免订阅逻辑散落在中间件内部。
//   - Middleware() 只负责读取当前状态并执行 CORS 校验，不再自行订阅配置事件。
type CorsController struct {
	mu             sync.RWMutex
	enabled        bool
	allowedOrigins string
	extraOrigins   string
}

// NewCorsController 使用初始配置构造一个 CORS 控制器。
func NewCorsController(enabled bool, allowedOrigins string) *CorsController {
	WarnLegacyOrigins("CORS", allowedOrigins)
	return &CorsController{
		enabled:        enabled,
		allowedOrigins: allowedOrigins,
	}
}

// WithTrustedOrigins binds listener-specific configured origins (such as
// KOMARI_PUBLIC_ORIGIN on the public listener or KOMARI_PANEL_ORIGIN on the
// panel listener) without merging public and panel origins across listeners.
func (ctrl *CorsController) WithTrustedOrigins(origins ...string) *CorsController {
	cleaned := make([]string, 0, len(origins))
	for _, origin := range origins {
		if trimmed := strings.TrimSpace(origin); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	ctrl.mu.Lock()
	ctrl.extraOrigins = strings.Join(cleaned, ",")
	ctrl.mu.Unlock()
	return ctrl
}

// Update 根据配置事件刷新 CORS 相关状态。返回是否有字段发生变化。
func (ctrl *CorsController) Update(event config.ConfigEvent) bool {
	ctrl.mu.Lock()
	defer ctrl.mu.Unlock()
	changed := false
	if ok, t := config.IsChangedT[bool](event, config.CorsOriginCheckEnabledKey); ok {
		ctrl.enabled = t
		changed = true
	}
	if ok, t := config.IsChangedT[string](event, config.CorsAllowedOriginsKey); ok {
		ctrl.allowedOrigins = t
		changed = true
	}
	return changed
}

func (ctrl *CorsController) snapshot() (bool, string, string) {
	ctrl.mu.RLock()
	defer ctrl.mu.RUnlock()
	return ctrl.enabled, ctrl.allowedOrigins, ctrl.extraOrigins
}

const corsMethods = "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS"
const corsRequestHeaders = "Origin, Content-Type, Authorization, Accept, If-None-Match, If-Match, If-Modified-Since, If-Unmodified-Since, Range, X-CSRF-Token, X-Requested-With, X-2FA-Code, X-Two-Factor-Code, Connect-Protocol-Version, Connect-Timeout-Ms, Connect-Content-Encoding, Connect-Accept-Encoding, Grpc-Timeout, Grpc-Encoding, Grpc-Accept-Encoding, X-Grpc-Web, X-User-Agent, X-Bind-Ticket, X-Komari-Workspace-Grant, X-Komari-Workspace-Token, X-Komari-Credential-Authorization, X-Komari-Signature, X-Komari-Timestamp, X-Komari-Nonce, X-Komari-Body-Mode, DPoP"
const corsResponseHeaders = "Content-Length, ETag, Last-Modified, Accept-Ranges, Content-Range, Retry-After, Bind-Ticket-Reason, X-Komari-Workspace-Identity, X-Komari-Storage-Policy, Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Connect-Content-Encoding, Connect-Accept-Encoding, DPoP-Nonce, WWW-Authenticate"

func varyBy(header http.Header, name string) {
	for _, value := range header.Values("Vary") {
		for _, field := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(field), name) || strings.TrimSpace(field) == "*" {
				return
			}
		}
	}
	header.Add("Vary", name)
}

func validPreflight(r *http.Request) bool {
	methods := r.Header.Values("Access-Control-Request-Method")
	if len(methods) != 1 {
		return false
	}
	switch methods[0] {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	allowed := make(map[string]bool)
	for _, name := range strings.Split(corsRequestHeaders, ",") {
		allowed[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, value := range r.Header.Values("Access-Control-Request-Headers") {
		for _, name := range strings.Split(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" && !allowed[name] {
				return false
			}
		}
	}
	return true
}

// Middleware 返回读取当前控制器状态的 gin 中间件。
func (ctrl *CorsController) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isAPIRequestPath(c.Request.URL.Path) {
			c.Next()
			return
		}

		corsEnabled, corsAllowedOrigins, extraOrigins := ctrl.snapshot()

		if !corsEnabled {
			c.Next()
			return
		}

		varyBy(c.Writer.Header(), "Origin")
		origin := c.GetHeader("Origin")
		allowOrigin := ""
		allowCredentials := false
		if len(c.Request.Header.Values("Origin")) == 1 && origin != "" {
			explicitMain, wildcardMain := MatchAllowlistOrigin(origin, corsAllowedOrigins)
			var explicitExtra, wildcardExtra bool
			if extraOrigins != "" {
				explicitExtra, wildcardExtra = MatchAllowlistOrigin(origin, extraOrigins)
			}
			if OriginMatchesRequest(origin, c.Request) || explicitMain || explicitExtra {
				allowOrigin = origin
				allowCredentials = true
			} else if wildcardMain || wildcardExtra {
				allowOrigin = "*"
			}
		}

		if allowOrigin != "" {
			c.Header("Access-Control-Allow-Origin", allowOrigin)
			if allowCredentials {
				c.Header("Access-Control-Allow-Credentials", "true")
			}
			c.Header("Access-Control-Expose-Headers", corsResponseHeaders)
		}

		if c.Request.Method == http.MethodOptions {
			varyBy(c.Writer.Header(), "Access-Control-Request-Method")
			varyBy(c.Writer.Header(), "Access-Control-Request-Headers")
			c.Header("Cache-Control", "no-store")
			if allowOrigin != "" && validPreflight(c.Request) {
				c.Header("Access-Control-Allow-Methods", corsMethods)
				c.Header("Access-Control-Allow-Headers", corsRequestHeaders)
				c.Header("Access-Control-Max-Age", "600")
				c.AbortWithStatus(http.StatusNoContent)
			} else {
				c.AbortWithStatus(http.StatusForbidden)
			}
			return
		}

		if (origin != "" || len(c.Request.Header.Values("Origin")) != 0) && allowOrigin == "" {
			AbortRequest(c, http.StatusForbidden, "request origin is not allowed")
			return
		}

		c.Next()
	}
}

func isAPIRequestPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/komari.")
}
