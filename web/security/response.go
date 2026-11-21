package security

import (
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
)

var rpcErrorWriter = connect.NewErrorWriter()

// AbortRequest preserves the caller's wire protocol even when a request is
// rejected before reaching a generated RPC handler. REST keeps its error shape.
func AbortRequest(c *gin.Context, status int, message string) {
	c.Abort()
	c.Header("Cache-Control", "no-store")
	if !strings.HasPrefix(c.Request.URL.Path, "/komari.") {
		c.JSON(status, gin.H{"error": message})
		return
	}
	code := connect.CodeInternal
	switch status {
	case http.StatusBadRequest:
		code = connect.CodeInvalidArgument
	case http.StatusUnauthorized:
		code = connect.CodeUnauthenticated
	case http.StatusForbidden:
		code = connect.CodePermissionDenied
	case http.StatusTooManyRequests:
		code = connect.CodeResourceExhausted
	case http.StatusServiceUnavailable:
		code = connect.CodeUnavailable
	}
	// Write handles Connect unary/stream, gRPC and gRPC-Web without reading the
	// body. A write failure means the peer has gone; never write a second error.
	_ = rpcErrorWriter.Write(c.Writer, c.Request, connect.NewError(code, errors.New(message)))
}
