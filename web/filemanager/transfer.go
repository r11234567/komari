package filemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/auditlog"
	logger "github.com/komari-monitor/komari/utils/log"
	"github.com/komari-monitor/komari/utils/safepath"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/workspace"
)

const (
	DefaultTransferChunkSize    int64 = 25 * 1024 * 1024
	MinTransferChunkSize        int64 = 1 * 1024 * 1024
	MaxTransferChunkSize        int64 = 128 * 1024 * 1024
	MaxUploadSize               int64 = 8 * 1024 * 1024 * 1024
	downloadChunkSizeContextKey       = "filemanager.download_chunk_size"
	uploadSessionTTL                  = 15 * time.Minute
	previewTokenTTL                   = 2 * time.Minute
	previewTokenMaxUses               = 16
	maxConcurrentUploads              = 4
)

var ErrTooManyUploads = errors.New("too many concurrent uploads")

type uploadSession struct {
	WorkspaceID string
	SystemUser  string
	closed      bool
	mu          sync.Mutex
	ID          string
	UUID        string
	Path        string
	Size        int64
	ChunkSize   int64
	NextOffset  int64
	ExpiresAt   time.Time
	slotHeld    bool
}

type previewToken struct {
	Grant         *workspace.Grant
	ClientUUID    string
	Path          string
	ExpiresAt     time.Time
	RemainingUses int
}

type downloadResponseOptions struct {
	// Filename is an optional ASCII-safe name used by external preview
	// services. It never affects which remote file is read.
	Filename string
	// OfficeCompatible omits the RFC 5987 filename* parameter. Some versions
	// of Office Online fail to consume a response that only exposes a UTF-8
	// filename for a non-ASCII document name.
	OfficeCompatible bool
}

var (
	uploadMu           sync.Mutex
	uploadSessions     = make(map[string]*uploadSession)
	uploadSlots        = make(chan struct{}, maxConcurrentUploads)
	cleanupOnce        sync.Once
	previewTokenMu     sync.Mutex
	previewTokens      = make(map[string]previewToken)
	downloadChunkMu    sync.RWMutex
	downloadChunkSizes = make(map[string]int64)
)

func cachedDownloadChunkSize(clientUUID string) int64 {
	if clientUUID == "" {
		return DefaultTransferChunkSize
	}
	downloadChunkMu.RLock()
	size := downloadChunkSizes[clientUUID]
	downloadChunkMu.RUnlock()
	if size < MinTransferChunkSize || size > MaxTransferChunkSize {
		return DefaultTransferChunkSize
	}
	return size
}

func downloadChunkSizeForRequest(c *gin.Context, clientUUID string) int64 {
	size := cachedDownloadChunkSize(clientUUID)
	requested := strings.TrimSpace(c.Query("chunk_size"))
	if requested == "" {
		return size
	}
	value, err := strconv.ParseInt(requested, 10, 64)
	if err == nil && value >= MinTransferChunkSize && value <= MaxTransferChunkSize {
		// Keep the safer value when the browser and Server have learned
		// different limits (for example after a process restart).
		return min(size, value)
	}
	return size
}

func rememberDownloadChunkSize(clientUUID string, size int64) {
	if clientUUID == "" || size < MinTransferChunkSize || size > MaxTransferChunkSize {
		return
	}
	downloadChunkMu.Lock()
	if existing := downloadChunkSizes[clientUUID]; existing >= MinTransferChunkSize && existing < size {
		downloadChunkMu.Unlock()
		return
	}
	if downloadChunkSizes == nil {
		downloadChunkSizes = make(map[string]int64)
	}
	downloadChunkSizes[clientUUID] = size
	downloadChunkMu.Unlock()
}

type persistedUploadSession struct {
	WorkspaceID string
	SystemUser  string    `json:"system_user,omitempty"`
	ID          string    `json:"id"`
	UUID        string    `json:"uuid"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ChunkSize   int64     `json:"chunk_size"`
	NextOffset  int64     `json:"next_offset"`
	ExpiresAt   time.Time `json:"expires_at"`
}

var uploadSessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func uploadSessionDirectory() (string, error) {
	absDir, err := filepath.Abs(filepath.Join("data", ".uploading"))
	if err != nil {
		return "", err
	}
	return filepath.Clean(absDir), nil
}

func uploadSessionPath(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "..") || !uploadSessionIDPattern.MatchString(id) {
		return "", errors.New("invalid upload session id")
	}
	baseDir, err := uploadSessionDirectory()
	if err != nil {
		return "", err
	}
	cleanRel := filepath.Clean("/" + ".komari-upload-" + id + ".json")
	filename := filepath.Base(cleanRel)
	target := filepath.Clean(filepath.Join(baseDir, filename))
	cleanBase := baseDir + string(filepath.Separator)
	if !strings.HasPrefix(target, cleanBase) {
		return "", errors.New("upload session path escapes base directory")
	}
	return safepath.JoinUnder(baseDir, filename)
}

func Upload(c *gin.Context) {
	cancel, ok := authorizeFileRequest(c)
	defer cancel()
	if !ok {
		return
	}

	clientUUID := c.Param("uuid")
	operation := c.Query("operation")
	if operation == "" {
		operation = "init"
	}
	switch operation {
	case "init":
		initRemoteUpload(c, clientUUID)
	case "chunk":
		uploadRemoteChunk(c, clientUUID)
	case "merge":
		mergeRemoteUpload(c, clientUUID)
	case "cancel":
		cancelUpload(c, clientUUID)
	default:
		api.RespondError(c, http.StatusNotFound, "unknown upload operation")
	}
}

func initRemoteUpload(c *gin.Context, clientUUID string) {
	var request struct {
		Path      string `json:"path" binding:"required"`
		Size      int64  `json:"size"`
		ChunkSize int64  `json:"chunk_size"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	request.Path = strings.TrimSpace(request.Path)
	if request.Path == "" {
		api.RespondError(c, http.StatusBadRequest, "path is required")
		return
	}
	if request.Size < 0 || request.Size > MaxUploadSize {
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("size must be between 0 and %d bytes", MaxUploadSize))
		return
	}
	chunkSize := request.ChunkSize
	if chunkSize == 0 {
		chunkSize = DefaultTransferChunkSize
	}
	if chunkSize <= 0 || chunkSize > MaxTransferChunkSize {
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("chunk_size must be between 1 and %d bytes", MaxTransferChunkSize))
		return
	}
	if request.Size == 0 {
		_, err := Call(c.Request.Context(), clientUUID, "create", map[string]any{
			"path": request.Path,
		}, CallOptions{Timeout: 60 * time.Second})
		if err != nil {
			respondTransferError(c, err)
			return
		}
		auditFileTransfer(c, "upload", clientUUID, request.Path)
		api.RespondSuccess(c, gin.H{"complete": true, "chunk_size": chunkSize})
		return
	}

	session, _, err := acquireUploadSession("", clientUUID, request.Path, request.Size, 0, requestWorkspaceID(c))
	if err != nil {
		respondTransferError(c, err)
		return
	}
	session.mu.Lock()
	session.ChunkSize = chunkSize
	session.SystemUser = workspace.FromContext(c.Request.Context()).Binding.SystemUser
	err = saveUploadSession(session)
	if err != nil {
		finishUploadSession(session)
	}
	session.mu.Unlock()
	if err != nil {
		respondTransferError(c, err)
		return
	}
	api.RespondSuccess(c, gin.H{
		"upload_id":        session.ID,
		"chunk_size":       chunkSize,
		"chunk_count":      uploadChunkCount(request.Size, chunkSize),
		"next_offset":      0,
		"next_chunk_index": 0,
		"complete":         false,
	})
}

func uploadRemoteChunk(c *gin.Context, clientUUID string) {
	uploadRemoteChunkStream(c, clientUUID)
}

func uploadRemoteChunkStream(c *gin.Context, clientUUID string) {
	uploadID := strings.TrimSpace(c.Query("upload_id"))
	if uploadID == "" {
		uploadID = strings.TrimSpace(c.GetHeader("X-Komari-Upload-ID"))
	}
	indexValue := strings.TrimSpace(c.Query("chunk_index"))
	if indexValue == "" {
		indexValue = strings.TrimSpace(c.GetHeader("X-Komari-Chunk-Index"))
	}
	index, err := strconv.ParseInt(indexValue, 10, 64)
	if err != nil {
		logger.Warnf("file-transfer", "upload chunk has invalid chunk_index client=%s upload_id=%q value=%q: %v", clientUUID, uploadID, indexValue, err)
		api.RespondError(c, http.StatusBadRequest, "chunk_index must be an integer")
		return
	}
	if uploadID == "" {
		logger.Warnf("file-transfer", "upload chunk missing upload_id client=%s", clientUUID)
		api.RespondError(c, http.StatusBadRequest, "upload_id is required")
		return
	}

	session, _, err := acquireUploadSession(uploadID, clientUUID, "", 0, 0, requestWorkspaceID(c))
	if err != nil {
		logger.Errorf("file-transfer", "upload chunk session lookup failed client=%s upload_id=%q: %v", clientUUID, uploadID, err)
		respondTransferError(c, err)
		return
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		respondTransferError(c, ErrUnknownToken)
		return
	}
	chunkSize := session.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultTransferChunkSize
	}
	if chunkSize > MaxTransferChunkSize {
		session.mu.Unlock()
		api.RespondError(c, http.StatusBadRequest, "upload session chunk size exceeds the maximum")
		return
	}
	chunkCount := uploadChunkCount(session.Size, chunkSize)
	if index < 0 || index >= chunkCount {
		session.mu.Unlock()
		api.RespondError(c, http.StatusBadRequest, "invalid chunk index")
		return
	}
	offset := index * chunkSize
	expectedSize := min(chunkSize, session.Size-offset)
	targetPath := session.Path
	sessionSize := session.Size
	sessionID := session.ID
	// A raw stream can legitimately stay open longer than the normal idle
	// session TTL on a slow uplink. Keep the persisted session alive for at
	// least the duration allowed for this transfer.
	session.ExpiresAt = time.Now().Add(streamCallTimeout)
	session.SystemUser = workspace.FromContext(c.Request.Context()).Binding.SystemUser
	err = saveUploadSession(session)
	session.mu.Unlock()
	if err != nil {
		respondTransferError(c, err)
		return
	}

	if c.Request.ContentLength >= 0 && c.Request.ContentLength != expectedSize {
		logger.Warnf("file-transfer", "upload chunk content length mismatch client=%s upload_id=%s chunk=%d got=%d want=%d", clientUUID, uploadID, index, c.Request.ContentLength, expectedSize)
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("chunk %d has size %d, want %d", index, c.Request.ContentLength, expectedSize))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, expectedSize+1)
	transfer, transferErr := newStreamTransfer(c.Request.Context(), clientUUID, streamUploadDirection, expectedSize)
	if transferErr != nil {
		logger.Errorf("file-transfer", "upload chunk relay allocation failed client=%s upload_id=%s chunk=%d size=%d: %v", clientUUID, uploadID, index, expectedSize, transferErr)
		respondTransferError(c, transferErr)
		return
	}
	defer transfer.close(nil)

	args := map[string]any{
		"path":           targetPath,
		"offset":         offset,
		"chunk_index":    index,
		"chunk_count":    chunkCount,
		"total_size":     sessionSize,
		"chunk_size":     chunkSize,
		"upload_id":      sessionID,
		"first":          index == 0,
		"final":          false,
		"transfer_id":    transfer.id,
		"transfer_token": transfer.token,
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	callDone := make(chan error, 1)
	go func() {
		_, callErr := Call(ctx, clientUUID, "upload_stream", args, CallOptions{Timeout: streamCallTimeout})
		callDone <- callErr
	}()

	bodyDone := make(chan streamCopyResult, 1)
	go func() {
		written, copyErr := copyExactPipe(transfer.writer, c.Request.Body, expectedSize)
		if copyErr == nil && written != expectedSize {
			copyErr = fmt.Errorf("upload body ended after %d of %d bytes", written, expectedSize)
		}
		if copyErr != nil {
			_ = transfer.writer.CloseWithError(copyErr)
		} else {
			_ = transfer.writer.Close()
		}
		bodyDone <- streamCopyResult{bytes: written, err: copyErr}
	}()

	var bodyResult streamCopyResult
	var callErr error
	bodyFinished, callFinished := false, false
	contextDone := c.Request.Context().Done()
	for !bodyFinished || !callFinished {
		select {
		case bodyResult = <-bodyDone:
			bodyFinished = true
			if bodyResult.err != nil {
				cancel()
				transfer.close(bodyResult.err)
			}
		case callErr = <-callDone:
			callFinished = true
			if callErr != nil {
				cancel()
				_ = c.Request.Body.Close()
				transfer.close(callErr)
			}
		case <-contextDone:
			cancel()
			_ = c.Request.Body.Close()
			transfer.close(c.Request.Context().Err())
			contextDone = nil
		}
	}
	if callErr != nil {
		// Closing the relay when the Agent fails can make the browser-side
		// copier report io.ErrClosedPipe. Preserve the Agent's original error;
		// it usually contains the useful HTTP status/body (for example a
		// proxy's 413) that would otherwise be hidden behind a generic 502.
		if bodyResult.err != nil {
			logger.Errorf("file-transfer", "upload chunk agent stream failed client=%s upload_id=%s chunk=%d offset=%d size=%d (body relay also failed after %d bytes: %v): %v", clientUUID, uploadID, index, offset, expectedSize, bodyResult.bytes, bodyResult.err, callErr)
		} else {
			logger.Errorf("file-transfer", "upload chunk agent stream failed client=%s upload_id=%s chunk=%d offset=%d size=%d: %v", clientUUID, uploadID, index, offset, expectedSize, callErr)
		}
		respondTransferError(c, callErr)
		return
	}
	if bodyResult.err != nil {
		logger.Errorf("file-transfer", "upload chunk body relay failed client=%s upload_id=%s chunk=%d bytes=%d want=%d: %v", clientUUID, uploadID, index, bodyResult.bytes, expectedSize, bodyResult.err)
		respondTransferError(c, bodyResult.err)
		return
	}

	session.mu.Lock()
	session.NextOffset = max(session.NextOffset, offset+expectedSize)
	nextOffset := session.NextOffset
	err = saveUploadSession(session)
	session.mu.Unlock()
	if err != nil {
		respondTransferError(c, err)
		return
	}
	api.RespondSuccess(c, gin.H{
		"received":    true,
		"chunk_index": index,
		"next_offset": nextOffset,
	})
}

func mergeRemoteUpload(c *gin.Context, clientUUID string) {
	var request struct {
		UploadID string `json:"upload_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		api.RespondError(c, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	session, _, err := acquireUploadSession(request.UploadID, clientUUID, "", 0, 0, requestWorkspaceID(c))
	if err != nil {
		respondTransferError(c, err)
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		respondTransferError(c, ErrUnknownToken)
		return
	}
	chunkSize := session.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultTransferChunkSize
	}
	_, err = Call(c.Request.Context(), clientUUID, "upload_commit", map[string]any{
		"path":        session.Path,
		"upload_id":   session.ID,
		"chunk_index": uploadChunkCount(session.Size, chunkSize),
		"chunk_count": uploadChunkCount(session.Size, chunkSize),
		"total_size":  session.Size,
		"chunk_size":  chunkSize,
		"offset":      session.Size,
		"first":       false,
		"final":       true,
	}, CallOptions{Timeout: 90 * time.Second})
	if err != nil {
		// Only a positive Agent result for this upload can prove completion.
		// An existing path with matching length may still be the old file.
		saveUploadSession(session)
		respondTransferError(c, err)
		return
	}
	finishUploadSession(session)
	auditFileTransfer(c, "upload", clientUUID, session.Path)
	api.RespondSuccess(c, gin.H{"complete": true})
}

func cancelUpload(c *gin.Context, clientUUID string) {
	id := strings.TrimSpace(c.Query("upload_id"))
	if id == "" {
		// Accept the JSON form used by older web clients as well as the query
		// parameter form used by the current endpoint.
		var request struct {
			UploadID string `json:"upload_id"`
		}
		if err := c.ShouldBindJSON(&request); err == nil {
			id = strings.TrimSpace(request.UploadID)
		}
	}
	if clientUUID == "" || id == "" {
		api.RespondError(c, http.StatusBadRequest, "uuid and upload_id are required")
		return
	}
	session, _, err := acquireUploadSession(id, clientUUID, "", 0, 0, requestWorkspaceID(c))
	if err != nil {
		respondTransferError(c, err)
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		respondTransferError(c, ErrUnknownToken)
		return
	}
	// Keep the cleanup record if the grant is revoked or the Agent disconnects
	// while an explicit cancel is in flight.
	session.ExpiresAt = time.Now()
	session.SystemUser = workspace.FromContext(c.Request.Context()).Binding.SystemUser
	if err := saveUploadSession(session); err != nil {
		respondTransferError(c, err)
		return
	}
	_, err = Call(c.Request.Context(), clientUUID, "upload_cancel", map[string]any{
		"upload_id": id,
		"path":      session.Path,
	}, CallOptions{Timeout: 30 * time.Second})
	if err == nil {
		finishUploadSession(session)
	} else {
		detachUploadSession(session)
	}
	api.RespondSuccess(c, gin.H{"cancelled": true})
}

func Download(c *gin.Context) {
	cancel, ok := authorizeFileRequest(c)
	defer cancel()
	if !ok {
		return
	}
	clientUUID := c.Param("uuid")
	path := strings.TrimSpace(c.Query("path"))
	if clientUUID == "" || path == "" {
		api.RespondError(c, http.StatusBadRequest, "uuid and path are required")
		return
	}
	downloadFile(c, clientUUID, path, downloadResponseOptions{})
}

func downloadFile(c *gin.Context, clientUUID, path string, options downloadResponseOptions) {
	event, err := Call(c.Request.Context(), clientUUID, "stat", map[string]any{"path": path}, CallOptions{Timeout: 60 * time.Second})
	if err != nil {
		logger.Errorf("file-transfer", "download stat failed client=%s path=%q: %v", clientUUID, path, err)
		respondTransferError(c, err)
		return
	}
	entries := event.GetEntries()
	if len(entries) == 0 {
		logger.Errorf("file-transfer", "download stat returned no entry client=%s path=%q", clientUUID, path)
		api.RespondError(c, http.StatusBadGateway, "invalid file metadata from agent")
		return
	}
	info := entries[0]
	if info.GetDirectory() {
		logger.Warnf("file-transfer", "download rejected for directory client=%s path=%q", clientUUID, path)
		api.RespondError(c, http.StatusBadRequest, "cannot download a directory")
		return
	}
	size := int64(info.GetSize())
	if size < 0 {
		api.RespondError(c, http.StatusBadGateway, "invalid negative file size from agent")
		return
	}
	modifiedAt := time.Time{}
	if info.GetModifiedAt() != nil {
		modifiedAt = info.GetModifiedAt().AsTime()
	}
	instance := info.GetInstanceId()
	etag := fmt.Sprintf("\"%x-%x\"", size, modifiedAt.UnixNano())
	if instance != "" {
		etag = fmt.Sprintf("\"%x-%x-%x\"", size, modifiedAt.UnixNano(), instance)
	}
	c.Header("ETag", etag)
	c.Header("Cache-Control", "no-store, no-transform")
	if value := c.GetHeader("If-Match"); value != "" && !matchesFileETag(value, etag, false) {
		api.RespondError(c, http.StatusPreconditionFailed, "file changed since this preview was opened")
		return
	}
	if matchesFileETag(c.GetHeader("If-None-Match"), etag, true) {
		c.Status(http.StatusNotModified)
		return
	}
	start, end := int64(0), size-1
	partial := false
	if rangeHeader := strings.TrimSpace(c.GetHeader("Range")); rangeHeader != "" && ifRangeAllowsRange(c.GetHeader("If-Range"), etag, modifiedAt) {
		var ok bool
		start, end, ok = parseSingleByteRange(rangeHeader, size)
		if !ok {
			c.Header("Content-Range", fmt.Sprintf("bytes */%d", size))
			api.RespondError(c, http.StatusRequestedRangeNotSatisfiable, "invalid byte range")
			return
		}
		partial = true
	}
	contentLength := int64(0)
	if size > 0 {
		contentLength = end - start + 1
	}

	actualName := info.GetName()
	if actualName == "" {
		actualName = filepath.Base(path)
	}
	name := actualName
	if options.Filename != "" {
		candidate := filepath.Base(strings.ReplaceAll(options.Filename, "\\", "/"))
		if candidate != "." && candidate != "" && candidate != "/" {
			// The response filename is only a hint for the consumer. Preserve the
			// real extension so MIME detection cannot be changed by the query.
			actualExtension := filepath.Ext(actualName)
			candidateExtension := filepath.Ext(candidate)
			if actualExtension != "" && !strings.EqualFold(candidateExtension, actualExtension) {
				candidate = strings.TrimSuffix(candidate, candidateExtension) + actualExtension
			}
			name = candidate
		}
	}
	disposition := "attachment"
	if c.Query("inline") == "1" || strings.EqualFold(c.Query("inline"), "true") {
		disposition = "inline"
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(actualName)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ext := strings.ToLower(filepath.Ext(actualName))
	ctLower := strings.ToLower(contentType)
	if ext == ".html" || ext == ".htm" || ext == ".xhtml" || ext == ".svg" || ext == ".xml" ||
		strings.Contains(ctLower, "html") || strings.Contains(ctLower, "svg") || strings.Contains(ctLower, "xml") ||
		strings.Contains(ctLower, "javascript") {
		disposition = "attachment"
		contentType = "application/octet-stream"
	}
	transferChunkSize := downloadChunkSizeForRequest(c, clientUUID)
	setHeaders := func() {
		headerChunkSize := transferChunkSize
		if value, exists := c.Get(downloadChunkSizeContextKey); exists {
			if requested, ok := value.(int64); ok && requested >= MinTransferChunkSize && requested <= MaxTransferChunkSize {
				headerChunkSize = requested
			}
		}
		c.Header("Content-Type", contentType)
		c.Header("Content-Disposition", formatDownloadContentDisposition(disposition, name, options.OfficeCompatible))
		c.Header("Accept-Ranges", "bytes")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
		c.Header("Content-Encoding", "identity")
		c.Header("Cache-Control", "no-store, no-transform")
		c.Header("X-Accel-Buffering", "no")
		c.Header("X-Komari-Transfer-Chunk-Size", strconv.FormatInt(headerChunkSize, 10))
		c.Header("ETag", etag)
		if !modifiedAt.IsZero() {
			c.Header("Last-Modified", modifiedAt.UTC().Format(http.TimeFormat))
		}
		if partial {
			c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			c.Status(http.StatusPartialContent)
		} else {
			c.Status(http.StatusOK)
		}
	}
	if c.Request.Method == http.MethodHead || contentLength == 0 {
		setHeaders()
		rememberDownloadChunkSize(clientUUID, transferChunkSize)
		auditFileTransfer(c, "download", clientUUID, path)
		return
	}

	// The RPC only opens one short-lived transfer at a time. The actual file
	// bytes are pushed by the Agent through the raw HTTP relay, so no complete
	// chunk is materialized in a Server JSON response.
	committed := false
	commitHeaders := func() {
		if committed {
			return
		}
		setHeaders()
		committed = true
	}
	snapshot := downloadSnapshot{size: size, modifiedAt: modifiedAt}
	if partial && c.Query("follow") == "1" && strings.HasPrefix(c.GetHeader("Range"), "bytes=-") {
		snapshot.followInstance = instance
	}
	if err := streamDownloadRelay(c, clientUUID, path, snapshot, start, contentLength, transferChunkSize, commitHeaders); err != nil {
		logger.Errorf("file-transfer", "download stream failed client=%s path=%q start=%d length=%d file_size=%d committed=%t: %v", clientUUID, path, start, contentLength, size, committed, err)
		if !committed {
			respondTransferError(c, err)
		} else {
			_ = c.Error(err)
		}
		return
	}
	auditFileTransfer(c, "download", clientUUID, path)
}

func matchesFileETag(header, etag string, weak bool) bool {
	for value := range strings.SplitSeq(header, ",") {
		value = strings.TrimSpace(value)
		if weak {
			value = strings.TrimPrefix(value, "W/")
		}
		if value == "*" || value == etag {
			return true
		}
	}
	return false
}

func ifRangeAllowsRange(value, etag string, modifiedAt time.Time) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if value == etag {
		return true
	}
	if timestamp, err := http.ParseTime(value); err == nil && !modifiedAt.IsZero() {
		return !modifiedAt.UTC().Truncate(time.Second).After(timestamp)
	}
	return false
}

func parseSingleByteRange(value string, size int64) (int64, int64, bool) {
	if size <= 0 || !strings.HasPrefix(strings.ToLower(value), "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimSpace(value[len("bytes="):])
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if strings.TrimSpace(parts[0]) == "" {
		suffix, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true
	}
	start, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if strings.TrimSpace(parts[1]) != "" {
		end, err = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

func CreatePreviewToken(c *gin.Context) {
	cancel, ok := authorizeFileRequest(c)
	defer cancel()
	if !ok {
		return
	}
	createPreviewToken(c)
}

func createPreviewToken(c *gin.Context) {
	clientUUID := c.Param("uuid")
	path := strings.TrimSpace(c.Query("path"))
	if clientUUID == "" || path == "" {
		api.RespondError(c, http.StatusBadRequest, "uuid and path are required")
		return
	}

	token := uuid.NewString()
	now := time.Now()
	previewTokenMu.Lock()
	for id, item := range previewTokens {
		if now.After(item.ExpiresAt) {
			delete(previewTokens, id)
		}
	}
	previewTokens[token] = previewToken{
		Grant:         workspace.FromContext(c.Request.Context()),
		ClientUUID:    clientUUID,
		Path:          path,
		ExpiresAt:     now.Add(previewTokenTTL),
		RemainingUses: previewTokenMaxUses,
	}
	previewTokenMu.Unlock()

	api.RespondSuccess(c, gin.H{
		"token":      token,
		"expires_in": int(previewTokenTTL.Seconds()),
	})
}

func PreviewDownload(c *gin.Context) {
	clientUUID := c.Param("uuid")
	token := strings.TrimSpace(c.Query("preview_token"))
	if clientUUID == "" || token == "" {
		api.RespondError(c, http.StatusBadRequest, "uuid and preview_token are required")
		return
	}

	previewTokenMu.Lock()
	item, ok := previewTokens[token]
	if ok && (item.ClientUUID != clientUUID || time.Now().After(item.ExpiresAt) || item.RemainingUses <= 0) {
		delete(previewTokens, token)
		ok = false
	} else if ok {
		item.RemainingUses--
		if item.RemainingUses <= 0 {
			delete(previewTokens, token)
		} else {
			previewTokens[token] = item
		}
	}
	previewTokenMu.Unlock()

	if !ok || item.Grant == nil || !item.Grant.Alive() {
		api.RespondError(c, http.StatusUnauthorized, "Invalid or expired preview token")
		return
	}
	bounded, cancel := workspace.Bind(c.Request.Context(), item.Grant)
	defer cancel()
	c.Request = c.Request.WithContext(bounded)
	// The token is bound to the exact path. Keeping the path out of the public
	// URL avoids nested URL encoding issues for non-ASCII filenames.
	// Keep the public URL and response filename ASCII-only. This avoids a
	// compatibility issue in Office Online's fetcher while the token still
	// binds the request to the original (possibly non-ASCII) path.
	filename := strings.TrimSpace(c.Query("filename"))
	if filename == "" {
		filename = "komari-preview"
	}
	downloadFile(c, clientUUID, item.Path, downloadResponseOptions{
		Filename:         filename,
		OfficeCompatible: true,
	})
}

func formatDownloadContentDisposition(disposition, name string, officeCompatible bool) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	if name == "." || name == "" || name == "/" {
		name = "download"
	}
	fallback := asciiDownloadFilename(name)
	header := mime.FormatMediaType(disposition, map[string]string{"filename": fallback})
	if officeCompatible || fallback == name {
		return header
	}
	return header + "; filename*=UTF-8''" + url.PathEscape(name)
}

func asciiDownloadFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if isSafeASCIIFilename(name) {
		return name
	}
	extension := filepath.Ext(name)
	if !isSafeASCIIFilename("file" + extension) {
		extension = ""
	}
	return "download" + extension
}

func isSafeASCIIFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character > 0x7e || character == '"' || character == '\\' {
			return false
		}
	}
	return true
}

func acquireUploadSession(id, clientUUID, path string, size, offset int64, workspaceIDs ...string) (*uploadSession, bool, error) {
	if id == "" {
		if offset != 0 {
			return nil, false, errors.New("upload_id is required after the first chunk")
		}
		select {
		case uploadSlots <- struct{}{}:
		default:
			return nil, false, ErrTooManyUploads
		}
		session := &uploadSession{
			ID:        uuid.NewString(),
			UUID:      clientUUID,
			Path:      path,
			Size:      size,
			ChunkSize: DefaultTransferChunkSize,
			ExpiresAt: time.Now().Add(uploadSessionTTL),
			WorkspaceID: func() string {
				if len(workspaceIDs) > 0 {
					return workspaceIDs[0]
				}
				return ""
			}(),
			slotHeld: true,
		}
		uploadMu.Lock()
		uploadSessions[session.ID] = session
		uploadMu.Unlock()
		ensureUploadSessionDirectory()
		return session, true, nil
	}

	uploadMu.Lock()
	session := uploadSessions[id]
	uploadMu.Unlock()
	if session != nil && removeExpiredUploadSession(session, time.Now()) {
		session = nil
	}
	if session == nil {
		if restoredSession := restoreUploadSession(id); restoredSession != nil {
			uploadMu.Lock()
			if existing := uploadSessions[id]; existing != nil {
				session = existing
			} else {
				if !time.Now().Before(restoredSession.ExpiresAt) {
					uploadMu.Unlock()
					return nil, false, ErrUnknownToken
				}
				select {
				case uploadSlots <- struct{}{}:
					restoredSession.slotHeld = true
					uploadSessions[id] = restoredSession
					session = restoredSession
				default:
					uploadMu.Unlock()
					return nil, false, ErrTooManyUploads
				}
			}
			uploadMu.Unlock()
		} else {
			return nil, false, ErrUnknownToken
		}
	}
	// Continuation requests identify the session by upload_id and client UUID;
	// path and size are only supplied when creating (or explicitly validating)
	// a session. Treat omitted values as wildcards so chunk/merge can resume.
	if (len(workspaceIDs) > 0 && (workspaceIDs[0] == "" || session.WorkspaceID != workspaceIDs[0])) || session.UUID != clientUUID ||
		(path != "" && session.Path != path) ||
		(size != 0 && session.Size != size) {
		return nil, false, errors.New("upload session does not match request")
	}
	return session, false, nil
}

func loadUploadSession(id string) (*persistedUploadSession, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "..") || !uploadSessionIDPattern.MatchString(id) {
		return nil, ErrUnknownToken
	}
	sessionPath, err := uploadSessionPath(id)
	if err != nil {
		return nil, ErrUnknownToken
	}
	baseDir, err := uploadSessionDirectory()
	if err != nil {
		return nil, ErrUnknownToken
	}
	cleanBase := baseDir + string(filepath.Separator)
	if !strings.HasPrefix(sessionPath, cleanBase) {
		return nil, ErrUnknownToken
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, ErrUnknownToken
	}
	var restored persistedUploadSession
	if err := json.Unmarshal(data, &restored); err != nil || restored.ID != id || restored.ExpiresAt.IsZero() {
		return nil, ErrUnknownToken
	}
	return &restored, nil
}

func finishUploadSession(session *uploadSession) {
	if session == nil {
		return
	}
	removeUploadSessionState(session.ID)
	detachUploadSession(session)
}

// The caller holds session.mu while retiring a session.
func detachUploadSession(session *uploadSession) {
	session.closed = true
	uploadMu.Lock()
	defer uploadMu.Unlock()
	if uploadSessions[session.ID] == session {
		delete(uploadSessions, session.ID)
		releaseUploadSlotLocked(session)
	}
}

func removeExpiredUploadSession(session *uploadSession, now time.Time) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || now.Before(session.ExpiresAt) {
		return session.closed
	}
	detachUploadSession(session)
	return true
}

func releaseUploadSlotLocked(session *uploadSession) {
	if session == nil || !session.slotHeld {
		return
	}
	session.slotHeld = false
	select {
	case <-uploadSlots:
	default:
	}
}

func saveUploadSession(session *uploadSession) error {
	if session.closed {
		return ErrUnknownToken
	}
	path, err := uploadSessionPath(session.ID)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".pending-upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(persistedUploadSession{
		ID:          session.ID,
		WorkspaceID: session.WorkspaceID,
		SystemUser:  session.SystemUser,
		UUID:        session.UUID,
		Path:        session.Path,
		Size:        session.Size,
		ChunkSize:   session.ChunkSize,
		NextOffset:  session.NextOffset,
		ExpiresAt:   session.ExpiresAt,
	})
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func removeUploadSessionState(id string) {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "..") || !uploadSessionIDPattern.MatchString(id) {
		return
	}
	path, err := uploadSessionPath(id)
	if err != nil {
		return
	}
	baseDir, err := uploadSessionDirectory()
	if err != nil {
		return
	}
	cleanBase := baseDir + string(filepath.Separator)
	if !strings.HasPrefix(path, cleanBase) {
		return
	}
	_ = os.Remove(path)
}

func StartUploadCleanup() {
	cleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			cursor := ""
			for now := range ticker.C {
				cursor = sweepExpiredUploads(now, cursor)
			}
		}()
	})
}

func sweepExpiredUploads(now time.Time, cursor string) string {
	uploadMu.Lock()
	sessions := make([]*uploadSession, 0, len(uploadSessions))
	for _, session := range uploadSessions {
		sessions = append(sessions, session)
	}
	uploadMu.Unlock()
	for _, session := range sessions {
		removeExpiredUploadSession(session, now)
	}

	dir, err := uploadSessionDirectory()
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	// Leave failed cancellations on disk for a later sweep, including across
	// panel restarts. Bound work per sweep and rotate past unavailable Agents.
	first := sort.Search(len(entries), func(i int) bool { return entries[i].Name() > cursor })
	attempts := 0
	for _, entry := range entries[first:] {
		name, prefixed := strings.CutPrefix(entry.Name(), ".komari-upload-")
		id, suffixed := strings.CutSuffix(name, ".json")
		if !prefixed || !suffixed || !entry.Type().IsRegular() {
			continue
		}
		session, err := loadUploadSession(id)
		if err != nil || now.Before(session.ExpiresAt) || session.SystemUser == "" {
			continue
		}
		if err := cancelAgentUpload(session); err == nil {
			removeUploadSessionState(id)
		} else if !errors.Is(err, ErrOffline) {
			logger.Warnf("file-transfer", "retrying expired upload cleanup client=%s upload_id=%s: %v", session.UUID, id, err)
		}
		attempts++
		if attempts == 4 {
			return entry.Name()
		}
	}
	return ""
}

func ensureUploadSessionDirectory() {
	if dir, err := uploadSessionDirectory(); err == nil {
		_ = os.MkdirAll(dir, 0o755)
	}
}

func restoreUploadSession(id string) *uploadSession {
	restored, err := loadUploadSession(id)
	if err != nil || !time.Now().Before(restored.ExpiresAt) {
		return nil
	}
	return &uploadSession{
		ID:          restored.ID,
		WorkspaceID: restored.WorkspaceID,
		SystemUser:  restored.SystemUser,
		UUID:        restored.UUID,
		Path:        restored.Path,
		Size:        restored.Size,
		ChunkSize:   restored.ChunkSize,
		NextOffset:  restored.NextOffset,
		ExpiresAt:   restored.ExpiresAt,
	}
}

func cancelAgentUpload(session *persistedUploadSession) error {
	if session == nil || session.UUID == "" || session.ID == "" || session.WorkspaceID == "" || session.SystemUser == "" || session.Path == "" {
		return ErrUnknownToken
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := callScoped(ctx, session.UUID, session.WorkspaceID, session.SystemUser, "upload_cancel", map[string]any{
		"upload_id": session.ID,
		"path":      session.Path,
	}, true, CallOptions{Timeout: 10 * time.Second})
	return err
}

func uploadChunkCount(size, chunkSize int64) int64 {
	if size <= 0 || chunkSize <= 0 {
		return 0
	}
	return (size + chunkSize - 1) / chunkSize
}

func parseInt64Query(c *gin.Context, name string) (int64, error) {
	value := c.Query(name)
	if value == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	return strconv.ParseInt(value, 10, 64)
}

func respondTransferError(c *gin.Context, err error) {
	if err == nil {
		err = errors.New("file transfer failed")
	}
	if err != nil {
		// Keep the public response concise, but retain the complete cause in
		// Gin's request error list so the access log identifies the failing
		// transfer stage instead of showing a bare 502.
		_ = c.Error(err)
	}
	status := http.StatusBadGateway
	switch {
	case strings.Contains(err.Error(), "download_stream: file changed while opening stream"):
		status = http.StatusConflict
	case isPayloadTooLargeError(err):
		// Preserve a proxy/body-size rejection so the browser can renegotiate
		// the logical chunk instead of retrying the same request forever.
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrUnsupported):
		status = http.StatusNotImplemented
	case isUnsupportedAgentFileOperation(err):
		// An older Agent (or an Agent with web control disabled) cannot execute
		// the real-time stream operation. This is a protocol/configuration
		// error, not a transient gateway failure; returning 501 prevents the
		// browser from retrying the same chunk forever.
		status = http.StatusNotImplemented
	case errors.Is(err, ErrOffline):
		status = http.StatusServiceUnavailable
	case errors.Is(err, ErrTimeout):
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		status = http.StatusRequestTimeout
	case errors.Is(err, ErrUnknownToken):
		status = http.StatusGone
	case errors.Is(err, ErrTooManyUploads):
		status = http.StatusTooManyRequests
	}
	message := err.Error()
	if isUnsupportedAgentFileOperation(err) || errors.Is(err, ErrUnsupported) {
		message = "Agent does not support real-time file transfer; update the Agent binary"
	}
	api.RespondError(c, status, message)
}

func isUnsupportedAgentFileOperation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unsupported file operation") ||
		strings.Contains(message, "does not support file operations") ||
		strings.Contains(message, "web control is disabled")
}

func auditFileTransfer(c *gin.Context, action, clientUUID, path string) {
	actor := c.GetString("uuid")
	auditlog.Log(c.ClientIP(), actor, fmt.Sprintf("file %s: %s:%s", action, clientUUID, path), "info")
}
