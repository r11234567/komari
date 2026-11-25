package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/internal/credentialaction"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/connection"
	"github.com/komari-monitor/komari/web/security"
	"github.com/komari-monitor/komari/web/signonauth"
)

// rpc2CompatibilityMethods is the complete read-only compatibility surface for
// third-party dashboards (11 public:* + 7 common:* + 2 rpc.* heartbeat methods).
// Admin, client, plugin, and audit-mutation methods are never exposed here.
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
	"common:getNodes":               {},
	"common:getNodesLatestStatus":   {},
	"common:getVersion":             {},
	"common:getRecords":             {},
	"common:getNodeRecentStatus":    {},
	"common:getPublicInfo":          {},
	"common:getMe":                  {},
	"rpc.ping":                      {},
	"rpc.version":                   {},
}

// rpc2TransportAllowed reports whether method may be reached through /api/rpc2.
func rpc2TransportAllowed(method string) bool {
	_, ok := rpc2CompatibilityMethods[method]
	return ok
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
	if method == "admin:editSettings" && c.Request.Method == http.MethodPost {
		ctx = credentialaction.WithToken(ctx, c.GetHeader(credentialaction.Header))
	}
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
	switch req.Method {
	case "public:queryMetrics", "public:getPingMetricStats", "public:getClientRecentRecords", "public:getRecordsByUUID", "public:getPingRecords", "common:getRecords", "common:getNodeRecentStatus":
		release, wait := security.AcquireCompatibilityHistoryRead(c)
		if release == nil {
			return rpc.ErrorResponse(req.ID, rpc.Unavailable, "historical reads are busy; retry shortly", map[string]int{"retry_after_seconds": int(wait.Seconds())})
		}
		defer release()
	}
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
	wsCtx, cancel := context.WithCancel(c.Request.Context())
	meta := buildContextMeta(c)
	conn.SetReadLimit(maxJSONRPCPostBytes)

	// Read control frames and detect disconnects while a query is in progress.
	// Dispatch and writes remain sequential so replies stay in request order.
	requests := make(chan compatibilityWSRequest, 32)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		readCompatibilityWSRequests(wsCtx, conn, requests)
	}()
	defer func() {
		cancel()
		conn.Close()
		<-readerDone
	}()

	for {
		select {
		case <-wsCtx.Done():
			return
		case message := <-requests:
			if wsCtx.Err() != nil {
				return
			}
			var response *rpc.JsonRpcResponse
			req := &message.request
			switch {
			case message.parseError != nil:
				response = rpc.ErrorResponse(nil, rpc.InvalidRequest, "bad request: "+message.parseError.Error(), nil)
			default:
				if jerr := req.Validate(); jerr != nil {
					response = jerr.ResponseWithID(req.ID)
				} else if !rpc2TransportAllowed(req.Method) {
					response = rpc2DeniedResponse(req)
				} else {
					response = dispatchWithSensitive(wsCtx, c, meta, req)
				}
			}
			if wsCtx.Err() != nil {
				return
			}
			if err := conn.GetConn().SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return
			}
			if err := conn.WriteJSON(response); err != nil {
				return
			}
		}
	}
}

type compatibilityWSRequest struct {
	request    rpc.JsonRpcRequest
	parseError error
}

func readCompatibilityWSRequests(ctx context.Context, conn *connection.SafeConn, queue chan<- compatibilityWSRequest) {
	for ctx.Err() == nil {
		var message compatibilityWSRequest
		if err := conn.ReadJSON(&message.request); err != nil {
			var syntaxErr *json.SyntaxError
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &syntaxErr) && !errors.As(err, &typeErr) {
				return
			}
			message.parseError = err
		}
		select {
		case <-ctx.Done():
			return
		case queue <- message:
		default:
			// A full queue must not prevent the reader from ending the session.
			closeFrame := websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many pending RPC requests")
			_ = conn.WriteControl(websocket.CloseMessage, closeFrame, time.Now().Add(time.Second))
			return
		}
	}
}

const (
	maxJSONRPCPostBytes = 256 << 10
	maxJSONRPCBatchSize = 256
)

func servePost(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONRPCPostBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, rpc.ErrorResponse(nil, rpc.InvalidRequest, "request body too large", nil))
			return
		}
		c.JSON(http.StatusBadRequest, rpc.ErrorResponse(nil, rpc.ParseError, "read body error", err.Error()))
		return
	}
	requests, jerr := rpc.ParseRequests(body)
	if jerr != nil {
		c.JSON(http.StatusBadRequest, jerr.Response())
		return
	}
	if len(requests) > maxJSONRPCBatchSize {
		c.JSON(http.StatusBadRequest, rpc.ErrorResponse(nil, rpc.InvalidRequest, "batch size exceeds limit", nil))
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
	if c != nil && c.GetBool("public_listener") {
		anon := rpc.NewAnonymousPrincipal()
		return &rpc.ContextMeta{
			Principal:      anon,
			Permission:     anon.PrimaryRole(),
			RemoteIP:       c.ClientIP(),
			UserAgent:      c.GetHeader("User-Agent"),
			TempShareValid: hasTempShareAccess(c),
		}
	}
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
		if session, err := c.Cookie(security.SessionCookieName); err == nil && session != "" {
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

// hasTempShareAccess delegates temporary share validation to web/api.HasTempAccess
// so redeemed temp shares and cookie checks share a single implementation.
func hasTempShareAccess(c *gin.Context) bool {
	return api.HasTempAccess(c)
}
