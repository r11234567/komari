package admin

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/komari-monitor/komari/internal/plugin"
	logger "github.com/komari-monitor/komari/utils/log"
	"github.com/komari-monitor/komari/web/backup"
	"github.com/komari-monitor/komari/web/upload"
)

func NewArchiveUploadHandler() *upload.Handler {
	upload.DefaultStore.StartCleanup()
	return upload.NewHandler(upload.DefaultStore, map[upload.Purpose]upload.Finalizer{
		upload.PurposeBackup: finalizeBackupUpload,
		upload.PurposePlugin: finalizePluginUpload,
		upload.PurposeTheme:  finalizeThemeUpload,
	})
}

func finalizeBackupUpload(session upload.Session) (upload.Result, error) {
	restoreLock, err := backup.AcquireRestoreLock()
	if err != nil {
		return upload.Result{}, err
	}
	archive, err := os.Open(session.ArchivePath)
	if err != nil {
		restoreLock.Release()
		return upload.Result{}, fmt.Errorf("open merged backup: %w", err)
	}
	if err := restoreLock.SaveUploadedBackup(archive, session.Metadata.Filename); err != nil {
		_ = archive.Close()
		restoreLock.Release()
		return upload.Result{}, err
	}
	if err := archive.Close(); err != nil {
		restoreLock.Release()
		return upload.Result{}, fmt.Errorf("close merged backup: %w", err)
	}
	backup.MarkRestartPending()

	go func() {
		// Keep restoreLock held through shutdown so no concurrent upload can race.
		_ = restoreLock
		logger.InfoArgs("admin-api", "Backup uploaded; requesting graceful shutdown to apply backup on startup...")
		time.Sleep(300 * time.Millisecond)
		backup.RequestGracefulRestart("admin_archive_backup_upload")
	}()

	return upload.Result{
		Message: "Backup uploaded successfully. The server is shutting down cleanly to apply the backup on startup (restart the process if not supervised).",
		Data: map[string]any{
			"path":                filepath.Join(".", "data", "backup.zip"),
			"restart_required":    true,
			"manual_restart_note": "If running outside a container or process supervisor, restart Komari manually after shutdown completes.",
		},
	}, nil
}

func finalizePluginUpload(session upload.Session) (upload.Result, error) {
	info, err := plugin.InstallOrStageZip(session.ArchivePath, "Chunked upload: "+session.Metadata.Filename, "", "", session.Actor)
	if err != nil {
		return upload.Result{}, err
	}
	auditPluginInstall(session.RemoteIP, session.Actor, info)
	return upload.Result{Message: pluginInstallMessage(info), Data: info}, nil
}

func finalizeThemeUpload(session upload.Session) (upload.Result, error) {
	info, err := extractAndValidateTheme(session.ArchivePath)
	if err != nil {
		return upload.Result{}, err
	}
	return upload.Result{Message: "主题上传成功", Data: info}, nil
}
