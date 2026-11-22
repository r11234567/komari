package dbcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	logger "github.com/komari-monitor/komari/utils/log"
)

const (
	generationsDirName      = "generations"
	currentManifestFileName = "current.json"
	backupArchiveFileName   = "backup.zip"
	fallbackPreRestoreDir   = ".pre-restore-backups"

	generationStatePendingPark    = "pending_park"
	generationStatePendingPublish = "pending_publish"
	generationStateActive         = "active"
)

type generationManifest struct {
	Version            int       `json:"version"`
	ActiveGeneration   string    `json:"active_generation"`
	PreviousGeneration string    `json:"previous_generation,omitempty"`
	State              string    `json:"state"`
	StagedEntries      []string  `json:"staged_entries,omitempty"`
	SecurityEpoch      uint64    `json:"security_epoch"`
	UpdatedAt          time.Time `json:"updated_at"`
}

var restoreFaultHook = struct {
	afterManifestPark    func() error
	afterManifestPublish func() error
}{}

func isReservedDataControlEntry(name string) bool {
	switch name {
	case generationsDirName,
		currentManifestFileName,
		securityEpochFileName,
		backupArchiveFileName,
		".uploading",
		fallbackPreRestoreDir:
		return true
	}
	return strings.HasPrefix(name, ".backup-upload-") ||
		strings.HasPrefix(name, ".current.json.tmp-") ||
		strings.HasPrefix(name, ".security-epoch.json.tmp-") ||
		strings.HasPrefix(name, ".komari-export-backup-")
}

func mergeReservedDataExcludes(dataDir string, exclude map[string]struct{}) map[string]struct{} {
	merged := make(map[string]struct{}, len(exclude)+8)
	for k := range exclude {
		merged[k] = struct{}{}
	}
	for _, fixed := range []string{
		generationsDirName,
		currentManifestFileName,
		securityEpochFileName,
		backupArchiveFileName,
		".uploading",
		fallbackPreRestoreDir,
	} {
		merged[filepath.Join(dataDir, fixed)] = struct{}{}
	}
	if entries, err := os.ReadDir(dataDir); err == nil {
		for _, entry := range entries {
			if isReservedDataControlEntry(entry.Name()) {
				merged[filepath.Join(dataDir, entry.Name())] = struct{}{}
			}
		}
	}
	return merged
}

func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func syncTree(root string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	var dirs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		return errors.Join(syncErr, closeErr)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		syncDir(dirs[i])
	}
	return nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	prefix := "." + filepath.Base(path) + ".tmp-"
	tmp, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keepTmp := false
	defer func() {
		if !keepTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	keepTmp = true
	syncDir(dir)
	return nil
}

func atomicWriteJSON(path string, value any, perm os.FileMode) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return atomicWriteFile(path, encoded, perm)
}

func readGenerationManifest(dataDir string) (*generationManifest, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, currentManifestFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var manifest generationManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("parse generation manifest: %w", err)
	}
	return &manifest, nil
}

func writeGenerationManifest(dataDir string, manifest generationManifest) error {
	manifest.Version = 1
	manifest.UpdatedAt = time.Now().UTC()
	return atomicWriteJSON(filepath.Join(dataDir, currentManifestFileName), manifest, 0o600)
}

func newGenerationID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
}

func isEmptyDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

func parkLiveDataEntries(dataDir, prevDir string) error {
	if err := os.MkdirAll(prevDir, 0o700); err != nil {
		return fmt.Errorf("create previous generation directory: %w", err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("list data directory entries: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if isReservedDataControlEntry(name) {
			continue
		}
		src := filepath.Join(dataDir, name)
		dst := filepath.Join(prevDir, name)
		if _, err := os.Lstat(dst); err == nil {
			if isEmptyDir(src) {
				_ = os.Remove(src)
				continue
			}
			dst = filepath.Join(prevDir, fmt.Sprintf("%s.partial-%d", name, time.Now().UTC().UnixNano()))
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("park live data entry %q: %w", name, err)
		}
	}
	syncDir(prevDir)
	syncDir(dataDir)
	return nil
}

func publishStagedEntries(dataDir, stageDir, prevDir string) error {
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list staged generation entries: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if isReservedDataControlEntry(name) {
			continue
		}
		src := filepath.Join(stageDir, name)
		dst := filepath.Join(dataDir, name)
		if _, err := os.Lstat(dst); err == nil {
			if isEmptyDir(dst) {
				_ = os.Remove(dst)
			} else if prevDir != "" {
				if err := os.MkdirAll(prevDir, 0o700); err != nil {
					return err
				}
				parked := filepath.Join(prevDir, fmt.Sprintf("%s.prepublish-%d", name, time.Now().UTC().UnixNano()))
				if err := os.Rename(dst, parked); err != nil {
					return fmt.Errorf("park colliding live entry %q before publish: %w", name, err)
				}
			}
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("publish restored entry %q: %w", name, err)
		}
	}
	syncDir(dataDir)
	return nil
}

func rollbackGenerationToLive(dataDir, stageDir, prevDir string) error {
	var rollbackErrs []error
	// Move any newly published entries back into stageDir before returning prevDir entries.
	if entries, err := os.ReadDir(dataDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if isReservedDataControlEntry(name) {
				continue
			}
			src := filepath.Join(dataDir, name)
			dst := filepath.Join(stageDir, name)
			if err := os.Rename(src, dst); err != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("revert published entry %q: %w", name, err))
			}
		}
	}
	if entries, err := os.ReadDir(prevDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			src := filepath.Join(prevDir, name)
			dst := filepath.Join(dataDir, name)
			if err := os.Rename(src, dst); err != nil {
				rollbackErrs = append(rollbackErrs, fmt.Errorf("restore previous entry %q: %w", name, err))
			}
		}
	} else if !os.IsNotExist(err) {
		rollbackErrs = append(rollbackErrs, err)
	}
	syncDir(dataDir)
	return errors.Join(rollbackErrs...)
}

func cleanupOrphanedGenerations(dataDir string) {
	genRoot := filepath.Join(dataDir, generationsDirName)
	entries, err := os.ReadDir(genRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(genRoot, entry.Name()))
	}
	// Clean stale atomic write temps inside dataDir.
	if top, err := os.ReadDir(dataDir); err == nil {
		for _, entry := range top {
			name := entry.Name()
			if strings.HasPrefix(name, ".current.json.tmp-") || strings.HasPrefix(name, ".security-epoch.json.tmp-") {
				_ = os.Remove(filepath.Join(dataDir, name))
			}
		}
	}
}

// RecoverPendingGeneration reconciles any generation switch interrupted by a
// crash or reboot before databases or runtime directories are initialized.
func RecoverPendingGeneration(dataDir string) error {
	if _, err := os.Stat(dataDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	manifest, err := readGenerationManifest(dataDir)
	if err != nil {
		return err
	}
	if manifest == nil || manifest.State == "" || manifest.State == generationStateActive {
		cleanupOrphanedGenerations(dataDir)
		return nil
	}

	genRoot := filepath.Join(dataDir, generationsDirName)
	stageDir := filepath.Join(genRoot, manifest.ActiveGeneration)
	prevDir := ""
	if manifest.PreviousGeneration != "" {
		prevDir = filepath.Join(genRoot, manifest.PreviousGeneration)
	}

	switch manifest.State {
	case generationStatePendingPark:
		if manifest.ActiveGeneration == "" {
			return errors.New("pending_park generation manifest missing active generation")
		}
		if _, err := os.Stat(stageDir); err != nil {
			return fmt.Errorf("pending_park staged generation %q missing: %w", manifest.ActiveGeneration, err)
		}
		if prevDir == "" {
			prevDir = filepath.Join(genRoot, newGenerationID("prev"))
			manifest.PreviousGeneration = filepath.Base(prevDir)
		}
		if err := parkLiveDataEntries(dataDir, prevDir); err != nil {
			return err
		}
		manifest.State = generationStatePendingPublish
		if err := writeGenerationManifest(dataDir, *manifest); err != nil {
			return err
		}
		fallthrough

	case generationStatePendingPublish:
		if manifest.SecurityEpoch > 0 {
			if err := writeSecurityEpochFile(dataDir, manifest.SecurityEpoch); err != nil {
				return fmt.Errorf("commit restored security epoch during recovery: %w", err)
			}
			SetCurrentSecurityEpoch(manifest.SecurityEpoch)
		}
		if err := publishStagedEntries(dataDir, stageDir, prevDir); err != nil {
			return err
		}
		if err := publishRestoredCustomDatabase(dataDir); err != nil {
			return err
		}
		syncDir(dataDir)
		manifest.State = generationStateActive
		manifest.PreviousGeneration = ""
		if err := writeGenerationManifest(dataDir, *manifest); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(dataDir, backupArchiveFileName))
		cleanupOrphanedGenerations(dataDir)
		logger.Infof("dbcore", "[restore] recovered interrupted generation switch into %s", manifest.ActiveGeneration)
		return nil

	default:
		return fmt.Errorf("unsupported generation manifest state %q", manifest.State)
	}
}

func commitStagedGeneration(dataDir, stageDir string, prevEpoch, newEpoch uint64, manifestCommitted *bool) error {
	genID := filepath.Base(stageDir)
	prevID := newGenerationID("prev")
	prevDir := filepath.Join(dataDir, generationsDirName, prevID)

	stagedList, err := os.ReadDir(stageDir)
	if err != nil {
		return fmt.Errorf("list staged generation before commit: %w", err)
	}
	names := make([]string, 0, len(stagedList))
	for _, entry := range stagedList {
		if !isReservedDataControlEntry(entry.Name()) {
			names = append(names, entry.Name())
		}
	}

	if err := syncTree(stageDir); err != nil {
		return fmt.Errorf("sync staged generation: %w", err)
	}

	manifest := generationManifest{
		Version:            1,
		ActiveGeneration:   genID,
		PreviousGeneration: prevID,
		State:              generationStatePendingPark,
		StagedEntries:      names,
		SecurityEpoch:      newEpoch,
	}
	if err := writeGenerationManifest(dataDir, manifest); err != nil {
		return fmt.Errorf("write pending_park manifest: %w", err)
	}
	if manifestCommitted != nil {
		*manifestCommitted = true
	}
	if err := parkLiveDataEntries(dataDir, prevDir); err != nil {
		if rbErr := rollbackGenerationToLive(dataDir, stageDir, prevDir); rbErr == nil {
			_ = os.RemoveAll(prevDir)
			_ = writeGenerationManifest(dataDir, generationManifest{State: generationStateActive, SecurityEpoch: prevEpoch})
			if manifestCommitted != nil {
				*manifestCommitted = false
			}
		}
		return err
	}
	if restoreFaultHook.afterManifestPark != nil {
		if err := restoreFaultHook.afterManifestPark(); err != nil {
			return err
		}
	}

	manifest.State = generationStatePendingPublish
	if err := writeGenerationManifest(dataDir, manifest); err != nil {
		return fmt.Errorf("write pending_publish manifest: %w", err)
	}
	if err := publishStagedEntries(dataDir, stageDir, prevDir); err != nil {
		rbErr := rollbackGenerationToLive(dataDir, stageDir, prevDir)
		if rbErr != nil {
			return fmt.Errorf("publish restored generation: %v; rollback: %w", err, rbErr)
		}
		_ = os.RemoveAll(prevDir)
		_ = writeGenerationManifest(dataDir, generationManifest{State: generationStateActive, SecurityEpoch: prevEpoch})
		if manifestCommitted != nil {
			*manifestCommitted = false
		}
		return err
	}
	if restoreFaultHook.afterManifestPublish != nil {
		if err := restoreFaultHook.afterManifestPublish(); err != nil {
			return err
		}
	}
	if err := publishRestoredCustomDatabase(dataDir); err != nil {
		rbErr := rollbackGenerationToLive(dataDir, stageDir, prevDir)
		if rbErr != nil {
			return fmt.Errorf("publish restored custom database: %v; rollback: %w", err, rbErr)
		}
		_ = os.RemoveAll(prevDir)
		_ = writeGenerationManifest(dataDir, generationManifest{State: generationStateActive, SecurityEpoch: prevEpoch})
		if manifestCommitted != nil {
			*manifestCommitted = false
		}
		return err
	}
	if newEpoch > 0 {
		if err := writeSecurityEpochFile(dataDir, newEpoch); err != nil {
			return fmt.Errorf("persist restored security epoch: %w", err)
		}
	}
	syncDir(dataDir)

	manifest.State = generationStateActive
	manifest.PreviousGeneration = ""
	if err := writeGenerationManifest(dataDir, manifest); err != nil {
		return fmt.Errorf("write active generation manifest: %w", err)
	}
	SetCurrentSecurityEpoch(newEpoch)
	if err := os.Remove(filepath.Join(dataDir, backupArchiveFileName)); err != nil && !os.IsNotExist(err) {
		logger.Errorf("dbcore", "[restore] generation active, failed to remove backup.zip: %v", err)
	}
	cleanupOrphanedGenerations(dataDir)
	return nil
}
