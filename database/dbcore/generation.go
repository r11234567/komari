package dbcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
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
	LiveEntries        []string  `json:"live_entries,omitempty"`
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

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
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
		if err := syncDir(dirs[i]); err != nil {
			return err
		}
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
	defer os.Remove(tmpPath)
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
	return syncDir(dir)
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
	manifest.Version = 2
	manifest.UpdatedAt = time.Now().UTC()
	return atomicWriteJSON(filepath.Join(dataDir, currentManifestFileName), manifest, 0o600)
}

func newGenerationID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
}

func generationEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !isReservedDataControlEntry(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

func validGenerationName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		filepath.Base(name) == name && !strings.ContainsAny(name, "/\\:") &&
		!isReservedDataControlEntry(name)
}

func generationEntryExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// The journal gives each move exactly two legitimate locations. A missing or
// ambiguous entry stops startup and leaves both generations available.
func moveGenerationEntries(from, to string, names []string) error {
	for _, dir := range []string{from, to} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("generation path %q is not a directory", dir)
		}
	}
	for _, name := range names {
		src, dst := filepath.Join(from, name), filepath.Join(to, name)
		atSource, err := generationEntryExists(src)
		if err != nil {
			return err
		}
		atDestination, err := generationEntryExists(dst)
		if err != nil {
			return err
		}
		if atSource == atDestination {
			return fmt.Errorf("generation entry %q must exist in exactly one of %s and %s", name, from, to)
		}
		if atSource {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("move generation entry %q: %w", name, err)
			}
		}
	}
	return errors.Join(syncDir(from), syncDir(to))
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

// RecoverPendingGeneration completes the durable restore journal before any
// database or HTTP listener opens. Failures retain the journal and both trees.
func RecoverPendingGeneration(dataDir string) error {
	manifest, err := readGenerationManifest(dataDir)
	if err != nil {
		return err
	}
	if manifest == nil || manifest.State == generationStateActive {
		cleanupOrphanedGenerations(dataDir)
		return nil
	}
	return resumeGenerationSwitch(dataDir, *manifest)
}

func resumeGenerationSwitch(dataDir string, manifest generationManifest) error {
	if manifest.Version != 2 || !validGenerationName(manifest.ActiveGeneration) ||
		!validGenerationName(manifest.PreviousGeneration) ||
		manifest.ActiveGeneration == manifest.PreviousGeneration || manifest.SecurityEpoch == 0 {
		return errors.New("invalid or unsupported pending generation journal")
	}
	for _, names := range [][]string{manifest.LiveEntries, manifest.StagedEntries} {
		for _, name := range names {
			if !validGenerationName(name) {
				return fmt.Errorf("invalid generation entry %q", name)
			}
		}
	}
	requiredDB := false
	for _, name := range manifest.StagedEntries {
		if name == "komari.db" {
			requiredDB = true
		}
	}
	if !requiredDB {
		return errors.New("staged generation does not include the primary database")
	}

	genRoot := filepath.Join(dataDir, generationsDirName)
	stageDir := filepath.Join(genRoot, manifest.ActiveGeneration)
	prevDir := filepath.Join(genRoot, manifest.PreviousGeneration)
	switch manifest.State {
	case generationStatePendingPark:
		staged, err := generationEntries(stageDir)
		if err != nil {
			return fmt.Errorf("inspect staged generation: %w", err)
		}
		if !slices.Equal(staged, manifest.StagedEntries) {
			return errors.New("staged generation does not match its journal")
		}
		if err := moveGenerationEntries(dataDir, prevDir, manifest.LiveEntries); err != nil {
			return fmt.Errorf("park live generation: %w", err)
		}
		if restoreFaultHook.afterManifestPark != nil {
			if err := restoreFaultHook.afterManifestPark(); err != nil {
				return err
			}
		}
		manifest.State = generationStatePendingPublish
		if err := writeGenerationManifest(dataDir, manifest); err != nil {
			return err
		}
	case generationStatePendingPublish:
	default:
		return fmt.Errorf("unsupported generation manifest state %q", manifest.State)
	}

	if err := moveGenerationEntries(stageDir, dataDir, manifest.StagedEntries); err != nil {
		return fmt.Errorf("publish staged generation: %w", err)
	}
	live, err := generationEntries(dataDir)
	if err != nil {
		return err
	}
	if !slices.Equal(live, manifest.StagedEntries) {
		return errors.New("published generation does not match its journal")
	}
	if err := validateRestoredSQLite(filepath.Join(dataDir, "komari.db"), "published komari.db"); err != nil {
		return err
	}
	if restoreFaultHook.afterManifestPublish != nil {
		if err := restoreFaultHook.afterManifestPublish(); err != nil {
			return err
		}
	}
	if err := publishRestoredCustomDatabase(dataDir); err != nil {
		return err
	}
	if err := writeSecurityEpochFile(dataDir, manifest.SecurityEpoch); err != nil {
		return err
	}
	// A restart after consuming the archive still sees the pending journal.
	// An active journal can therefore never apply the same staged archive again.
	if err := os.Remove(filepath.Join(dataDir, backupArchiveFileName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	manifest.State = generationStateActive
	manifest.PreviousGeneration = ""
	if err := writeGenerationManifest(dataDir, manifest); err != nil {
		return err
	}
	SetCurrentSecurityEpoch(manifest.SecurityEpoch)
	cleanupOrphanedGenerations(dataDir)
	return nil
}

func commitStagedGeneration(dataDir, stageDir string, newEpoch uint64, manifestCommitted *bool) error {
	names, err := generationEntries(stageDir)
	if err != nil {
		return err
	}
	liveNames, err := generationEntries(dataDir)
	if err != nil {
		return err
	}
	if err := syncTree(stageDir); err != nil {
		return fmt.Errorf("sync staged generation: %w", err)
	}

	prevID := newGenerationID("prev")
	genRoot := filepath.Join(dataDir, generationsDirName)
	if err := os.Mkdir(filepath.Join(genRoot, prevID), 0o700); err != nil {
		return err
	}
	if err := syncDir(genRoot); err != nil {
		return err
	}
	manifest := generationManifest{
		Version:            2,
		ActiveGeneration:   filepath.Base(stageDir),
		PreviousGeneration: prevID,
		State:              generationStatePendingPark,
		StagedEntries:      names,
		LiveEntries:        liveNames,
		SecurityEpoch:      newEpoch,
	}
	// A directory sync error can follow a successful manifest rename. Retain
	// the staged data from this point since the next startup may see it.
	*manifestCommitted = true
	if err := writeGenerationManifest(dataDir, manifest); err != nil {
		return fmt.Errorf("write pending_park manifest: %w", err)
	}
	return resumeGenerationSwitch(dataDir, manifest)
}
