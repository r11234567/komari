package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/pkg/rpc"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/web/api"
)

const maxChunkRequestSize = ChunkSize + 1*1024*1024

type Result struct {
	Message string
	Data    any
}

type Finalizer func(Session) (Result, error)

type Handler struct {
	Store      *Store
	Finalizers map[Purpose]Finalizer
}

func NewHandler(store *Store, finalizers map[Purpose]Finalizer) *Handler {
	return &Handler{Store: store, Finalizers: finalizers}
}

func (h *Handler) Init(c *gin.Context) {
	owner, ok := interactiveOwner(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var request struct {
		Purpose  Purpose `json:"purpose" binding:"required"`
		Size     int64   `json:"size" binding:"required"`
		Filename string  `json:"filename"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if _, ok := h.Finalizers[request.Purpose]; !ok {
		api.RespondError(c, http.StatusBadRequest, "unsupported upload purpose")
		return
	}
	session, err := h.Store.Init(owner, request.Purpose, request.Filename, request.Size)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	api.RespondSuccess(c, gin.H{
		"upload_id":  session.ID,
		"chunk_size": ChunkSize,
	})
}

func (h *Handler) Chunk(c *gin.Context) {
	owner, ok := interactiveOwner(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxChunkRequestSize)
	defer func() {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	}()
	uploadID := c.PostForm("upload_id")
	index, err := strconv.ParseInt(c.PostForm("chunk_index"), 10, 64)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, "chunk_index must be an integer")
		return
	}
	chunk, _, err := c.Request.FormFile("chunk_data")
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("get chunk data: %v", err))
		return
	}
	defer chunk.Close()
	if err := h.Store.SaveChunk(owner, uploadID, index, chunk); err != nil {
		h.respondUploadError(c, err)
		return
	}
	api.RespondSuccess(c, gin.H{"received": true, "chunk_index": index})
}

func (h *Handler) Merge(c *gin.Context) {
	owner, ok := interactiveOwner(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var request struct {
		UploadID string `json:"upload_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	result, err := h.Store.Finalize(owner, request.UploadID, func(session Session) (Result, error) {
		finalize, ok := h.Finalizers[session.Metadata.Purpose]
		if !ok {
			return Result{}, fmt.Errorf("unsupported upload purpose")
		}
		return finalize(session)
	})
	if err != nil {
		h.respondUploadError(c, err)
		return
	}

	api.RespondSuccessMessage(c, result.Message, result.Data)
}

func (h *Handler) Cancel(c *gin.Context) {
	owner, ok := interactiveOwner(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var request struct {
		UploadID string `json:"upload_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	if err := h.Store.Cancel(owner, request.UploadID); err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	api.RespondSuccess(c, gin.H{})
}

func (h *Handler) respondUploadError(c *gin.Context, err error) {
	if errors.Is(err, ErrNotFound) {
		api.RespondError(c, http.StatusNotFound, "upload not found or expired")
		return
	}
	api.RespondError(c, http.StatusBadRequest, err.Error())
}

func interactiveOwner(c *gin.Context) (string, bool) {
	p := api.GetPrincipal(c)
	session, _ := c.Cookie("session_token")
	if p == nil || p.Type != rpc.PrincipalUser || p.IsAPIKey || !p.HasRole(rpc.RoleAdmin) || session == "" {
		api.RespondError(c, http.StatusUnauthorized, "interactive admin session required")
		return "", false
	}
	id, _, err := accounts.GetSessionIdentity(session)
	if err != nil || id != p.UserUUID {
		api.RespondError(c, http.StatusUnauthorized, "invalid admin session")
		return "", false
	}
	key := sha256.Sum256([]byte(id + "\x00" + session))
	return hex.EncodeToString(key[:]), true
}
