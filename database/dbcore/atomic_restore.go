package dbcore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/internal/datadir"
	"github.com/komari-monitor/komari/pkg/historyexport"
	logger "github.com/komari-monitor/komari/utils/log"
)

const restoreInbox = ".restore-inbox"
const rejectedRestores = ".restore-rejected"

var releaseDataOwner func()

var atomicRestoreTestHook func(string) error

func atomicRestoreCheckpoint(name string) error {
	if atomicRestoreTestHook != nil {
		return atomicRestoreTestHook(name)
	}
	return nil
}

// initializeDataLayout runs before the database, stores and listeners open.
func initializeDataLayout() error {
	root, err := filepath.Abs(datadir.Root)
	if err != nil {
		return err
	}
	lockDir := filepath.Join(root, ".restore-lock")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := historyexport.LockMaintenance(ctx, lockDir)
	if err != nil {
		return fmt.Errorf("lock Komari data directory: %w", err)
	}
	layout, err := activateQueuedRestore(root)
	if err != nil {
		release()
		return err
	}
	if err := pruneSupersededGenerations(layout); err != nil {
		logger.Warnf("dbcore", "[restore] reclaim superseded generations: %v", err)
	}
	datadir.Pin(layout)
	releaseDataOwner = release
	return nil
}

func closeDataOwner() {
	if releaseDataOwner != nil {
		releaseDataOwner()
		releaseDataOwner = nil
	}
}

// Local primary SQLite files can take part in the same publication as the
// data tree. An external database has a separate transaction boundary.
func restorePrimaryPath(root string) (string, error) {
	if !flags.IsSQLite() {
		return "", errors.New("atomic backup restore requires a local SQLite primary database")
	}
	path := configuredSQLiteFilePath()
	if path == "" && strings.TrimSpace(flags.DatabaseFile) != "" {
		return "", errors.New("atomic backup restore requires a persistent primary SQLite file")
	}
	if path == "" || path == filepath.Clean("./data/komari.db") {
		return "komari.db", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", errors.New("atomic backup restore requires the primary SQLite file inside the data directory")
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	if isReservedDataControlEntry(first) {
		return "", errors.New("primary SQLite file points into the restore control directory")
	}
	return rel, nil
}

func SupportsAtomicRestore() error {
	root, err := filepath.Abs(datadir.Root)
	if err != nil {
		return err
	}
	_, err = restorePrimaryPath(root)
	return err
}

// PendingRestore reports whether startup would apply an archive. Offline
// maintenance uses this before any call that initializes the primary store.
func PendingRestore(root string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(root, backupArchiveFileName)); !os.IsNotExist(err) {
		return err == nil, err
	}
	active, err := datadir.Open(root)
	if errors.Is(err, datadir.ErrPendingLegacyRestore) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	ids, err := queuedRestoreIDs(filepath.Join(root, restoreInbox))
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if active.Pointer.Version != datadir.Version || id != active.Pointer.Generation {
			return true, nil
		}
	}
	return false, nil
}

func activateQueuedRestore(root string) (datadir.Layout, error) {
	active, err := datadir.Open(root)
	if errors.Is(err, datadir.ErrPendingLegacyRestore) {
		// Only finish an in-flight version 2 publication. New uploads use a
		// pointer replacement and never enter the journal's move states.
		if err := RecoverPendingGeneration(root); err != nil {
			return datadir.Layout{}, err
		}
		active, err = datadir.Open(root)
	}
	if err != nil {
		return datadir.Layout{}, err
	}

	inbox := filepath.Join(root, restoreInbox)
	ids, err := queuedRestoreIDs(inbox)
	if err != nil {
		logger.Errorf("dbcore", "[restore] inspect pending archive: %v", err)
		return active, nil
	}
	if len(ids) == 1 && active.Pointer.Version == datadir.Version && active.Pointer.Generation == ids[0] {
		if err := consumeRestoreArchive(inbox, filepath.Join(inbox, ids[0]+".zip")); err != nil {
			logger.Errorf("dbcore", "[restore] clean up published archive: %v", err)
			return active, nil
		}
		ids = nil
	}
	if len(ids) == 0 {
		id, err := claimRestore(root, inbox)
		if err != nil {
			logger.Errorf("dbcore", "[restore] retain current data: %v", err)
			return active, nil
		}
		if id == "" {
			return active, nil
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 {
		logger.Errorf("dbcore", "[restore] multiple queued restore archives; retain current data")
		return active, nil
	}
	id := ids[0]
	archive := filepath.Join(inbox, id+".zip")

	// Before publication every error leaves the old generation selected.
	// After attempting publication, preserve the staged tree even when a
	// directory sync fails: the renamed pointer may already be visible.
	publishing, err := publishQueuedRestore(active, id, archive)
	if err != nil {
		if publishing {
			return datadir.Layout{}, err
		}
		logger.Errorf("dbcore", "[restore] retain current data: %v", err)
		if rejectErr := rejectRestore(root, inbox, archive); rejectErr != nil {
			logger.Errorf("dbcore", "[restore] keep rejected archive queued: %v", rejectErr)
		}
		return active, nil
	}
	if err := consumeRestoreArchive(inbox, archive); err != nil {
		logger.Errorf("dbcore", "[restore] clean up published archive: %v", err)
	}
	return datadir.Open(root)
}

func queuedRestoreIDs(inbox string) ([]string, error) {
	entries, err := os.ReadDir(inbox)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".zip")
		if !ok || !datadir.ValidGeneration(id) || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("unexpected restore inbox entry %q", entry.Name())
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func claimRestore(root, inbox string) (string, error) {
	path := filepath.Join(root, backupArchiveFileName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("staged restore archive is not a regular file")
	}
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return "", err
	}
	var entropy [16]byte
	rand.Read(entropy[:])
	id := "gen-" + hex.EncodeToString(entropy[:])
	if err := datadir.Rename(path, filepath.Join(inbox, id+".zip")); err != nil {
		return "", err
	}
	return id, errors.Join(syncDir(inbox), syncDir(root))
}

func rejectRestore(root, inbox, archive string) error {
	dir := filepath.Join(root, rejectedRestores)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := datadir.Rename(archive, filepath.Join(dir, filepath.Base(archive))); err != nil {
		return err
	}
	return errors.Join(syncDir(dir), syncDir(inbox), syncDir(root))
}

func consumeRestoreArchive(inbox, archive string) error {
	err := os.Remove(archive)
	if err == nil || os.IsNotExist(err) {
		return syncDir(inbox)
	}
	return err
}

func publishQueuedRestore(active datadir.Layout, id, archive string) (publishing bool, err error) {
	primary, err := restorePrimaryPath(active.Root)
	if err != nil {
		return false, err
	}
	parent := filepath.Join(active.Root, datadir.Generations)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return false, err
	}
	if err := verifyRestoreVolume(active.Root, parent); err != nil {
		return false, err
	}
	stage := filepath.Join(parent, id)
	// An interrupted pre-commit attempt may have left an incomplete stage.
	// Its inbox ID cannot be the active generation at this point.
	if err := os.RemoveAll(stage); err != nil {
		return false, err
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		return false, err
	}
	defer func() {
		if !publishing {
			_ = os.RemoveAll(stage)
		}
	}()

	liveDB := filepath.Join(active.Dir, primary)
	epoch, err := prepareStagedGeneration(archive, stage, stage, func(staged string) (uint64, error) {
		return reconcileAtomicRestore(active, staged, liveDB)
	})
	if err != nil {
		return false, err
	}
	if primary != "komari.db" {
		if err := atomicCopySQLiteFile(filepath.Join(stage, "komari.db"), filepath.Join(stage, primary)); err != nil {
			return false, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	releasePublication, err := historyexport.LockGenerationPublication(ctx, active.Root)
	cancel()
	if err != nil {
		return false, fmt.Errorf("quiesce generation consumers before restore: %w", err)
	}
	defer releasePublication()

	queueDir := filepath.Join(active.Dir, "exports-v2")
	if _, err := os.Stat(queueDir); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		release, err := historyexport.LockMaintenance(ctx, queueDir)
		cancel()
		if err != nil {
			return false, fmt.Errorf("quiesce export worker before restore: %w", err)
		}
		defer release()
	} else if !os.IsNotExist(err) {
		return false, err
	}
	previous, err := writePreRestoreSnapshotFrom(active.Root, active.Dir, archive, liveDB)
	if err != nil {
		return false, err
	}

	if err := errors.Join(syncTree(stage), syncDir(parent), syncDir(active.Root)); err != nil {
		return false, fmt.Errorf("sync unpublished restore: %w", err)
	}
	pointer := datadir.Pointer{Version: datadir.Version, Generation: id, Epoch: epoch}
	if active.Pointer.Version == datadir.Version {
		pointer.PreviousGeneration = active.Pointer.Generation
	}
	encoded, err := json.Marshal(pointer)
	if err != nil {
		return false, err
	}
	if err := atomicRestoreCheckpoint("before-pointer"); err != nil {
		return false, err
	}
	publishing, err = replaceDurableFile(filepath.Join(active.Root, datadir.Manifest), encoded, 0o600)
	if err != nil {
		return publishing, fmt.Errorf("publish data generation: %w", err)
	}
	if err := atomicRestoreCheckpoint("after-pointer"); err != nil {
		return true, err
	}
	logger.Infof("dbcore", "[restore] selected generation %s (epoch=%d); previous data saved to %s", id, epoch, previous)
	return true, nil
}

// Reclaim only automatically named restore directories once a durable pointer
// identifies both the active tree and its immediate predecessor. The original
// flat data and pre-restore archives are outside this directory.
func pruneSupersededGenerations(layout datadir.Layout) error {
	if layout.Pointer.Version != datadir.Version || layout.Pointer.PreviousGeneration == "" {
		return nil
	}
	if pending, err := PendingRestore(layout.Root); pending || err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := historyexport.LockGenerationPublication(ctx, layout.Root)
	if err != nil {
		return err
	}
	defer release()
	// A prior startup may have stopped immediately after replacing current.json.
	if err := syncDir(layout.Root); err != nil {
		return err
	}
	path := filepath.Join(layout.Root, datadir.Generations)
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	var removeErr error
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == layout.Pointer.Generation || name == layout.Pointer.PreviousGeneration {
			continue
		}
		hexID, ok := strings.CutPrefix(name, "gen-")
		if !ok || len(hexID) != 32 || strings.Trim(hexID, "0123456789abcdef") != "" {
			continue
		}
		if err := root.RemoveAll(name); err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	return errors.Join(removeErr, syncDir(path))
}

func verifyRestoreVolume(root, parent string) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("restore generations path is not a directory")
	}
	f, err := os.CreateTemp(root, ".restore-volume-*")
	if err != nil {
		return err
	}
	source := f.Name()
	defer os.Remove(source)
	if err := f.Close(); err != nil {
		return err
	}
	destination := filepath.Join(parent, filepath.Base(source))
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("restore generations must be on the data volume: %w", err)
	}
	return os.Remove(destination)
}

func reconcileAtomicRestore(active datadir.Layout, stage, liveDB string) (uint64, error) {
	path := filepath.Join(stage, "komari.db")
	dsn := readOnlyRestoreDSN(path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return 0, err
	}
	var check string
	err = errors.Join(db.QueryRow("PRAGMA integrity_check").Scan(&check), db.Close())
	if err != nil {
		return 0, err
	}
	if check != "ok" {
		return 0, fmt.Errorf("metadata SQLite integrity check failed: %s", check)
	}
	portable, err := sql.Open("sqlite3", "file:"+(&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()+"?mode=rw")
	if err != nil {
		return 0, err
	}
	if err := errors.Join(NormalizePortableMetricConfig(context.Background(), portable), portable.Close()); err != nil {
		return 0, err
	}
	carry, liveEpoch, err := collectLiveSecurityStateFrom(active.Dir, liveDB)
	if err != nil {
		return 0, err
	}
	rootEpoch, err := readSecurityEpochFile(active.Root)
	if err != nil {
		return 0, err
	}
	// The first-run guide may already have created an empty database and a
	// signing key. Those bootstrap records do not replace the backed-up identity.
	epoch := max(liveEpoch, rootEpoch, active.Pointer.Epoch)
	if !carry.primaryHasUsers && len(carry.agentCredentials) == 0 && active.Pointer.Version == 0 && epoch <= 1 {
		carry.agentCredentials = nil
		carry.controlPlaneKeys = nil
		carry.controlPlanePrivate = nil
	}
	if err := carry.rewrapAgentRecovery(active.Dir, stage); err != nil {
		return 0, err
	}
	return applyRestoreSecurityStateToDB(path, epoch, carry)
}

func readOnlyRestoreDSN(path string) string {
	return "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() + "?mode=ro"
}
