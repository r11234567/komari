package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/internal/credentialaction"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/signonauth"
)

// rpc2CompatibilityMethods is the complete compatibility surface for
// third-party dashboards. Admin and client methods are reached over Connect;
// exposing them here would create a second, less constrained management API.
var rpc2CompatibilityMethods = map[string]struct{}{
	"public:getMe":                  {},
	"public:getNodesInformation":    {},
	"public:getPublicSettings":      {},
	"public:getVersion":             {},
	"public:getClientRecentRecords": {},
	"public:getRecordsByUUID":       {},
	"public:getPingRecords":         {},
	"public:getPublicPingTasks":     {},
	"public:listMetricDefinitions":  {},
	"public:queryMetrics":           {},
	"public:getPingMetricStats":     {},
	"public:recordVisitorEvent":     {},
	"common:getNodes":               {},
	"common:getNodesLatestStatus":   {},
}

// rpc2TransportAllowed reports whether method may be reached through /api/rpc2.
// Plugin-owned methods are admitted only after the plugin runtime registered
// them; an admin namespace never becomes reachable by naming a plugin method.
func rpc2TransportAllowed(method string) bool {
	if _, ok := rpc2CompatibilityMethods[method]; ok {
		return true
	}
	namespace, _, _ := strings.Cut(strings.ToLower(method), ":")
	switch namespace {
	case "admin", "client", "public", "common", "rpc":
		return false
	}
	return rpc.IsPluginMethod(method)
}

func rpc2DeniedResponse(req *rpc.JsonRpcRequest) *rpc.JsonRpcResponse {
	return rpc.ErrorResponse(req.ID, rpc.PermissionDenied,
		"method is not available on the compatibility transport", map[string]string{"method": req.Method})
}

// OnRpcRequest 是 /api/rpc2 的统一入口：GET 升级为 WebSocket，POST 处理单条/批量 JSON-RPC。
func OnRpcRequest(c *gin.Context) {
	// GET -> WebSocket
	if c.Request.Method == http.MethodGet {
		serveWebSocket(c)
		return
	}

	if c.Request.Method != http.MethodPost {
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
		return
	}
	servePost(c)
}

// CallFromGin 供传统 gin handler / 路由桥转调 RPC 方法。
// 复用 IdentityMiddleware 已识别的 principal；未识别时兜底调用 IdentifyPrincipal。
func CallFromGin(c *gin.Context, method string, params any) *rpc.JsonRpcResponse {
	meta := buildContextMeta(c)
	req := &rpc.JsonRpcRequest{Version: rpc.RPC_VERSION, Method: method, Params: params}
	token, _ := c.Cookie(signonauth.CookieName)
	ctx := signonauth.WithToken(c.Request.Context(), token)
	return dispatchWithSensitive(ctx, c, meta, req)
}

// dispatchWithSensitive 在统一分发前对敏感方法补充二次验证，使各调用入口行为一致。
// 对已通过命名空间权限校验的敏感方法，要求调用方满足敏感操作 2FA。
// API keys cannot satisfy step-up verification. Dispatch remains authoritative.
//
// 2FA code 按"每请求"提取:优先取自本条 RPC 请求的 params(2fa_code/two_factor_code/otp),
// 这对 WebSocket 长连接尤其重要——每条敏感消息携带新鲜的 TOTP 码,避免连接级握手码过期或被复用;
// 缺失时回退到 X-2FA-Code / X-Two-Factor-Code 请求头与 query(REST/直连场景)。
//
// 若请求已被 RequireSensitive2FA 中间件校验过(sensitive_2fa_verified),则跳过,避免重复校验。
func dispatchWithSensitive(ctx context.Context, c *gin.Context, meta *rpc.ContextMeta, req *rpc.JsonRpcRequest) *rpc.JsonRpcResponse {
	if meta != nil && meta.Principal != nil && (c == nil || !c.GetBool("sensitive_2fa_verified")) &&
		rpc.IsSensitive(req.Method) && rpc.CheckPrincipal(meta.Principal, req.Method) {
		// Native proofs belong to one RPC message, never to a WebSocket handshake.
		token, _ := rpc.GetParamAs[string](req, "authorization_token")
		if token == "" && c != nil && c.Request.Method == http.MethodPost {
			token = c.GetHeader(credentialaction.Header)
		}
		if token != "" {
			var params struct {
				Clients []string `json:"clients"`
			}
			if req.Method != "admin:exec" || req.BindParams(&params) != nil {
				return rpc.ErrorResponse(req.ID, rpc.PermissionDenied, "unsupported credential action", nil)
			}
			bound := rpc.NewContextWithMeta(ctx, meta)
			if _, err := credentialaction.Consume(bound, token, credential.SensitiveOperation, credentialaction.ScopedResource("rpc:"+req.Method, params.Clients...)); err != nil {
				return rpc.ErrorResponse(req.ID, rpc.PermissionDenied, err.Error(), nil)
			}
			return Dispatch(ctx, meta, req)
		}
		code := extractRequestTwoFACode(req)
		if code == "" && c != nil {
			code = headerOrQueryTwoFACode(c)
		}
		if err := api.VerifySensitive2FACore(meta.Principal.UserUUID, code, meta.Principal.IsAPIKey); err != nil {
			return rpc.ErrorResponse(req.ID, rpc.PermissionDenied, err.Error(), nil)
		}
	}
	return Dispatch(ctx, meta, req)
}

// extractRequestTwoFACode 从单条 RPC 请求的命名参数中提取 2FA code。
// 仅支持对象(map)形式的 params;按 2fa_code / two_factor_code / otp 顺序查找。
func extractRequestTwoFACode(req *rpc.JsonRpcRequest) string {
	for _, key := range []string{"2fa_code", "two_factor_code", "otp"} {
		if v, ok := rpc.GetParamAs[string](req, key); ok && v != "" {
			return v
		}
	}
	return ""
}

// headerOrQueryTwoFACode 从请求头 / query 兜底提取 2FA code(不读取 body,避免消费请求体)。
func headerOrQueryTwoFACode(c *gin.Context) string {
	if code := c.GetHeader("X-2FA-Code"); code != "" {
		return code
	}
	if code := c.GetHeader("X-Two-Factor-Code"); code != "" {
		return code
	}
	for _, key := range []string{"2fa_code", "two_factor_code", "otp"} {
		if code := c.Query(key); code != "" {
			return code
		}
	}
	return ""
}

func serveWebSocket(c *gin.Context) {
	conn, err := api.UpgradeSafeConn(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "Failed to upgrade to WebSocket." + err.Error()})
		return
	}
	defer conn.Close()

	meta := buildContextMeta(c)
	for {
		var req rpc.JsonRpcRequest
		if err := conn.ReadJSON(&req); err != nil {
			var se *json.SyntaxError
			var ute *json.UnmarshalTypeError
			if errors.As(err, &se) || errors.As(err, &ute) {
				conn.WriteJSON(rpc.ErrorResponse(nil, rpc.InvalidRequest, "bad request: "+err.Error(), nil))
				continue
			}
			// 其它视为连接/IO 错误，结束循环
			break
		}
		if jerr := req.Validate(); jerr != nil {
			conn.WriteJSON(jerr.ResponseWithID(req.ID))
			continue
		}
		if !rpc2TransportAllowed(req.Method) {
			conn.WriteJSON(rpc2DeniedResponse(&req))
			continue
		}
		// 同步写：SafeConn 内部有锁，串行写避免响应乱序与并发竞态。
		conn.WriteJSON(dispatchWithSensitive(context.Background(), c, meta, &req))
	}
}

func servePost(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, rpc.ErrorResponse(nil, rpc.ParseError, "read body error", err.Error()))
		return
	}
	requests, jerr := rpc.ParseRequests(body)
	if jerr != nil {
		c.JSON(http.StatusBadRequest, jerr.Response())
		return
	}
	meta := buildContextMeta(c)

	responses := make([]*rpc.JsonRpcResponse, 0, len(requests))
	for _, rreq := range requests {
		if !rpc2TransportAllowed(rreq.Method) {
			responses = append(responses, rpc2DeniedResponse(rreq))
			continue
		}
		responses = append(responses, dispatchWithSensitive(c.Request.Context(), c, meta, rreq))
	}
	// 单条直接对象，批量数组（符合 JSON-RPC 2.0）。
	if len(responses) == 1 {
		c.JSON(http.StatusOK, responses[0])
	} else {
		c.JSON(http.StatusOK, responses)
	}
}

// buildContextMeta 从 gin.Context 构建 *rpc.ContextMeta。
// 复用 IdentityMiddleware 已识别的 principal(api.GetPrincipal)；若未识别则兜底调用
// api.IdentifyPrincipal。填充 principal、Permission(兼容)、User、各 UUID、token 等字段。
func buildContextMeta(c *gin.Context) *rpc.ContextMeta {
	// 优先读取中间件已识别的 principal；未识别时兜底自行识别(如 /api/rpc2 请求)。
	p := api.GetPrincipal(c)
	if p == nil {
		p = api.IdentifyPrincipal(c)
	}

	meta := &rpc.ContextMeta{
		Principal:  p,
		Permission: p.PrimaryRole(), // 兼容现有 handler 与 Dispatch
		RemoteIP:   c.ClientIP(),
		UserAgent:  c.GetHeader("User-Agent"),
	}

	// 根据主体类型填充具体字段。
	switch p.Type {
	case rpc.PrincipalUser:
		meta.UserUUID = p.UserUUID
		if session, err := c.Cookie("session_token"); err == nil && session != "" {
			meta.SessionToken = session
			if user, err := accounts.GetUserBySession(session); err == nil {
				meta.User = &user
			}
		}
	case rpc.PrincipalAgent:
		meta.ClientUUID = p.ClientUUID
		// 尝试提取 client token(用于某些 handler 需要原始 token 的场景)。
		// 优先查询参数 ?Authorization=<token>，再尝试 Bearer header。
		if token := c.Query("Authorization"); token != "" {
			meta.ClientToken = token
		} else if auth := c.GetHeader("Authorization"); auth != "" && len(auth) > len("Bearer ") {
			meta.ClientToken = auth[len("Bearer "):]
		}
	}

	// 临时分享访问许可。
	meta.TempShareValid = hasTempShareAccess(c)
	return meta
}

// hasTempShareAccess 校验 temp_key cookie 是否为有效的临时分享访问许可。
func hasTempShareAccess(c *gin.Context) bool {
	tempKey, err := c.Cookie("temp_key")
	if err != nil || tempKey == "" {
		return false
	}
	expireAt, err := config.GetAs[int64]("tempory_share_token_expire_at", 0)
	if err != nil {
		return false
	}
	allowKey, err := config.GetAs[string]("tempory_share_token", "")
	if err != nil || allowKey == "" || tempKey != allowKey {
		return false
	}
	return expireAt >= time.Now().Unix()
}
