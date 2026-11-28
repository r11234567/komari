package filemanager

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/web/api"
)

const maxEditableFileSize = 2 << 20

func Edit(c *gin.Context) {
	cancel, ok := authorizeFileRequest(c)
	defer cancel()
	if !ok {
		return
	}
	path := c.Query("path")
	if strings.TrimSpace(path) == "" {
		api.RespondError(c, http.StatusBadRequest, "file path is required")
		return
	}
	version := c.GetHeader("If-Match")
	if version == "" {
		api.RespondError(c, http.StatusPreconditionRequired, "the file's original content version is required")
		return
	}
	digest, err := parseEditVersion(version)
	if err != nil {
		api.RespondError(c, http.StatusBadRequest, err.Error())
		return
	}
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxEditableFileSize)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			api.RespondError(c, http.StatusRequestEntityTooLarge, "edited file exceeds 2 MiB")
		} else {
			api.RespondError(c, http.StatusBadRequest, "could not read edited file")
		}
		return
	}
	clientUUID := c.Param("uuid")
	_, err = Call(c.Request.Context(), clientUUID, "replace", map[string]any{
		"path": path, "content": data, "expected_digest": digest,
	})
	if err != nil {
		if err.Error() == "file changed since it was opened" {
			api.RespondError(c, http.StatusPreconditionFailed, err.Error())
		} else {
			respondTransferError(c, err)
		}
		return
	}
	auditFileTransfer(c, "edit", clientUUID, path)
	c.Status(http.StatusNoContent)
}

func parseEditVersion(version string) ([]byte, error) {
	if len(version) == 2+2*sha256.Size && version[0] == '"' && version[len(version)-1] == '"' {
		if digest, err := hex.DecodeString(version[1 : len(version)-1]); err == nil {
			return digest, nil
		}
	}
	return nil, errors.New("If-Match must be a quoted SHA-256 digest")
}
