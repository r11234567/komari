package dbcore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/datadir"
	"github.com/komari-monitor/komari/pkg/historyexport"
	"github.com/komari-monitor/komari/utils/secureconfig"
	"github.com/stretchr/testify/require"
)

func newAtomicRestoreFixture(t *testing.T) (root, backupDB string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "data")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	createRestoreSQLite(t, filepath.Join(root, "komari.db"), "live")
	backupDB = filepath.Join(base, "backup.db")
	createRestoreSQLite(t, backupDB, "restored")
	writeRestoreArchive(t, filepath.Join(root, backupArchiveFileName), backupDB)
	return root, backupDB
}

func assertRestoreValue(t *testing.T, dir, want string) {
	t.Helper()
	path := filepath.Join(dir, "komari.db")
	db, err := sql.Open("sqlite3", readOnlyRestoreDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow("SELECT value FROM state").Scan(&value); err != nil || value != want {
		t.Fatalf("%s value=%q, want %q (err=%v)", path, value, want, err)
	}
}

func TestAtomicRestoreInterruptionHelper(t *testing.T) {
	root := os.Getenv("KOMARI_RESTORE_TEST_ROOT")
	if root == "" {
		return
	}
	stopAt := os.Getenv("KOMARI_RESTORE_TEST_STOP")
	atomicRestoreTestHook = func(step string) error {
		if step != stopAt {
			return nil
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(root), "ready"), []byte(step), 0o600); err != nil {
			return err
		}
		for {
			time.Sleep(time.Second)
		}
	}
	if _, err := activateQueuedRestore(root); err != nil {
		t.Fatal(err)
	}
	t.Fatal("did not reach restore interruption point " + stopAt)
}

func TestAtomicRestoreCanRestartAfterProcessDeath(t *testing.T) {
	for _, step := range []string{"extract-entry", "before-pointer", "after-pointer"} {
		t.Run(step, func(t *testing.T) {
			root, _ := newAtomicRestoreFixture(t)
			if err := writeSecurityEpochFile(root, 5); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAtomicRestoreInterruptionHelper$")
			child.Env = append(os.Environ(), "KOMARI_RESTORE_TEST_ROOT="+root, "KOMARI_RESTORE_TEST_STOP="+step)
			var output strings.Builder
			child.Stdout, child.Stderr = &output, &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			}()
			marker := filepath.Join(filepath.Dir(root), "ready")
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					_ = child.Wait()
					waited = true
					t.Fatalf("child did not reach %s: %s", step, output.String())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil {
				t.Fatal("interrupted child exited successfully")
			}
			waited = true

			before, err := datadir.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if step == "after-pointer" {
				assertRestoreValue(t, before.Dir, "restored")
			} else if before.Dir != root {
				t.Fatalf("published before %s: %+v", step, before)
			}
			assertRestoreValue(t, root, "live")
			after, err := activateQueuedRestore(root)
			if err != nil {
				t.Fatal(err)
			}
			if after.Pointer.Epoch != 6 || after.Dir == root {
				t.Fatalf("unexpected activation: %+v", after)
			}
			assertRestoreValue(t, after.Dir, "restored")
			assertRestoreValue(t, root, "live")
			again, err := activateQueuedRestore(root)
			if err != nil || again != after {
				t.Fatalf("replayed a consumed archive: %+v, %v", again, err)
			}
		})
	}
}

func TestAtomicRestoreExtractionFailureKeepsPreviousGeneration(t *testing.T) {
	root, backupDB := newAtomicRestoreFixture(t)
	original, err := activateQueuedRestore(root)
	if err != nil || original.Dir == root {
		t.Fatalf("initial generation = %+v, %v", original, err)
	}
	writeRestoreArchive(t, filepath.Join(root, backupArchiveFileName), backupDB)
	atomicRestoreTestHook = func(step string) error {
		if step == "extract-entry" {
			return errors.New("injected disk write failure")
		}
		return nil
	}
	defer func() { atomicRestoreTestHook = nil }()
	got, err := activateQueuedRestore(root)
	if err != nil || got != original {
		t.Fatalf("failed restore selected %+v instead of %+v: %v", got, original, err)
	}
	assertRestoreValue(t, original.Dir, "restored")
	assertRestoreValue(t, root, "live")
	rejected, err := os.ReadDir(filepath.Join(root, rejectedRestores))
	if err != nil || len(rejected) != 1 {
		t.Fatalf("rejected archives = %v, %v", rejected, err)
	}
	generations, err := os.ReadDir(filepath.Join(root, datadir.Generations))
	if err != nil || len(generations) != 1 {
		t.Fatalf("partial generation retained: %v, %v", generations, err)
	}
}

func TestAtomicRestoreLeavesEarlierEpochAndFencesExportWorker(t *testing.T) {
	root, backupDB := newAtomicRestoreFixture(t)
	queuePath := filepath.Join(root, "exports-v2")
	ctx := context.Background()
	oldQueue, err := historyexport.OpenWorkerQueue(ctx, queuePath)
	if err != nil {
		t.Fatal(err)
	}
	defer oldQueue.Close()

	first, err := activateQueuedRestore(root)
	if err != nil || first.Dir == root {
		t.Fatalf("initial activation: %+v, %v", first, err)
	}
	if _, err := oldQueue.List(ctx, "owner"); !errors.Is(err, historyexport.ErrQueueReplaced) {
		t.Fatalf("stale worker could use previous queue: %v", err)
	}
	newQueue, err := historyexport.OpenWorkerQueue(ctx, queuePath)
	if err != nil {
		t.Fatal(err)
	}
	defer newQueue.Close()
	if newQueue.Dir != filepath.Join(first.Dir, "exports-v2") {
		t.Fatalf("worker directory = %s, active data = %s", newQueue.Dir, first.Dir)
	}
	writeRestoreArchive(t, filepath.Join(root, backupArchiveFileName), backupDB)
	next, err := activateQueuedRestore(root)
	if err != nil || next.Pointer.Epoch != first.Pointer.Epoch+1 || next.Dir == first.Dir {
		t.Fatalf("second activation: %+v, previous=%+v, err=%v", next, first, err)
	}
	if _, err := newQueue.List(ctx, "owner"); !errors.Is(err, historyexport.ErrQueueReplaced) {
		t.Fatalf("worker kept generation one after the next restore: %v", err)
	}
	assertRestoreValue(t, first.Dir, "restored")
	assertRestoreValue(t, next.Dir, "restored")
}

func TestAtomicRestoreRefusesExternalPrimaryFile(t *testing.T) {
	root, _ := newAtomicRestoreFixture(t)
	external := filepath.Join(t.TempDir(), "external.db")
	createRestoreSQLite(t, external, "external")
	before := flags.DatabaseFile
	flags.DatabaseFile = external
	defer func() { flags.DatabaseFile = before }()
	if _, err := restorePrimaryPath(root); err == nil {
		t.Fatal("accepted external primary database for atomic publication")
	}
	layout, err := activateQueuedRestore(root)
	if err != nil || layout.Dir != root {
		t.Fatalf("unsafe restore changed the active layout: %+v, %v", layout, err)
	}
	assertRestoreValue(t, root, "live")
	db, err := sql.Open("sqlite3", readOnlyRestoreDSN(external))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow("SELECT value FROM state").Scan(&value); err != nil || value != "external" {
		t.Fatalf("external database value=%q, err=%v", value, err)
	}
}

func TestAtomicRestoreCanApplyNextUploadAfterPriorArchiveWasPublished(t *testing.T) {
	root, backupDB := newAtomicRestoreFixture(t)
	atomicRestoreTestHook = func(step string) error {
		if step == "after-pointer" {
			return errors.New("interrupted before consuming the archive")
		}
		return nil
	}
	_, err := activateQueuedRestore(root)
	atomicRestoreTestHook = nil
	if err == nil {
		t.Fatal("expected an interruption")
	}
	first, err := datadir.Open(root)
	if err != nil || first.Pointer.Epoch != 2 {
		t.Fatalf("first generation: %+v, %v", first, err)
	}
	writeRestoreArchive(t, filepath.Join(root, backupArchiveFileName), backupDB)
	next, err := activateQueuedRestore(root)
	if err != nil || next.Dir == first.Dir || next.Pointer.Epoch != 3 {
		t.Fatalf("subsequent restore = %+v, err=%v", next, err)
	}
}

func TestAtomicRestoreReclaimsSupersededGenerations(t *testing.T) {
	root, backupDB := newAtomicRestoreFixture(t)
	first, err := activateQueuedRestore(root)
	require.NoError(t, err)
	require.Empty(t, first.Pointer.PreviousGeneration)

	writeRestoreArchive(t, filepath.Join(root, backupArchiveFileName), backupDB)
	second, err := activateQueuedRestore(root)
	require.NoError(t, err)
	require.Equal(t, first.Pointer.Generation, second.Pointer.PreviousGeneration)

	manual := filepath.Join(root, datadir.Generations, "manual-copy")
	require.NoError(t, os.Mkdir(manual, 0o700))
	writeRestoreArchive(t, filepath.Join(root, backupArchiveFileName), backupDB)
	require.NoError(t, pruneSupersededGenerations(second))
	require.DirExists(t, first.Dir)

	third, err := activateQueuedRestore(root)
	require.NoError(t, err)
	require.Equal(t, second.Pointer.Generation, third.Pointer.PreviousGeneration)
	require.NoError(t, pruneSupersededGenerations(third))
	require.NoDirExists(t, first.Dir)
	require.DirExists(t, manual)
	assertRestoreValue(t, second.Dir, "restored")
	assertRestoreValue(t, third.Dir, "restored")
}

func TestAtomicRestorePreservesBackupIdentityOnlyBeforeFirstRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed bool
		sameAgent bool
	}{
		{name: "first-run guide"},
		{name: "existing installation with no agents", installed: true},
		{name: "existing installation with an upgraded agent", installed: true, sameAgent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, backupDB := newAtomicRestoreFixture(t)
			t.Setenv("KOMARI_SECRET_KEY", "synthetic restore fixture key")
			sealed, err := secureconfig.EncryptString("test signing material")
			require.NoError(t, err)
			live, err := sql.Open("sqlite3", filepath.Join(root, "komari.db"))
			require.NoError(t, err)
			liveModel, err := restoreDBModel(live)
			require.NoError(t, err)
			require.NoError(t, liveModel.AutoMigrate(&models.User{}, &models.AgentCredential{}, &models.ControlPlaneKey{}, &models.ControlPlanePrivateKey{}))
			if tc.installed {
				require.NoError(t, liveModel.Create(&models.User{UUID: "owner", Username: "owner", Passwd: "synthetic-hash"}).Error)
			}
			if tc.sameAgent {
				require.NoError(t, liveModel.Create(&models.AgentCredential{
					Client: "active", AgentPublicKey: "active-public", Generation: 8,
					RefreshTokenHash: "live-refresh", DPoPJWTRequired: true, SecurityEpoch: 1,
				}).Error)
			}
			require.NoError(t, liveModel.Create(&models.ControlPlaneKey{KeyID: "live-key", PublicKey: "live-key-public"}).Error)
			require.NoError(t, liveModel.Create(&models.ControlPlanePrivateKey{KeyID: "live-key", Sealed: sealed}).Error)
			require.NoError(t, live.Close())

			snapshot, err := sql.Open("sqlite3", backupDB)
			require.NoError(t, err)
			backupModel, err := restoreDBModel(snapshot)
			require.NoError(t, err)
			require.NoError(t, backupModel.AutoMigrate(&models.AgentCredential{}, &models.ControlPlaneKey{}, &models.ControlPlanePrivateKey{}))
			revokedAt := time.Now().UTC()
			backupAgents := []models.AgentCredential{
				{Client: "active", AgentPublicKey: "active-public", Generation: 7, RefreshTokenHash: "fixture-refresh", SecurityEpoch: 1},
				{Client: "revoked", AgentPublicKey: "revoked-public", RevokedAt: &revokedAt, RevokedReason: "fixture revoked"},
			}
			require.NoError(t, backupModel.Create(&backupAgents).Error)
			require.NoError(t, backupModel.Create(&models.ControlPlaneKey{KeyID: "backup-key", PublicKey: "backup-key-public"}).Error)
			require.NoError(t, backupModel.Create(&models.ControlPlanePrivateKey{KeyID: "backup-key", Sealed: sealed}).Error)
			require.NoError(t, snapshot.Close())
			writeRestoreArchive(t, filepath.Join(root, backupArchiveFileName), backupDB)

			layout, err := activateQueuedRestore(root)
			require.NoError(t, err)
			require.Greater(t, layout.Pointer.Epoch, uint64(1))
			db, err := sql.Open("sqlite3", readOnlyRestoreDSN(filepath.Join(layout.Dir, "komari.db")))
			require.NoError(t, err)
			defer db.Close()
			agents, err := readLiveAgentCredentials(db)
			require.NoError(t, err)
			require.NotNil(t, agents["revoked"].RevokedAt)
			require.Equal(t, "fixture revoked", agents["revoked"].RevokedReason)
			var keyID string
			require.NoError(t, db.QueryRow("SELECT key_id FROM control_plane_keys").Scan(&keyID))
			switch {
			case tc.sameAgent:
				active := agents["active"]
				require.Nil(t, active.RevokedAt)
				require.True(t, active.DPoPJWTRequired)
				require.Equal(t, uint64(8), active.Generation)
				require.Equal(t, "live-refresh", active.RefreshTokenHash)
				require.Less(t, active.SecurityEpoch, layout.Pointer.Epoch)
				require.Equal(t, "live-key", keyID)
			case tc.installed:
				require.NotNil(t, agents["active"].RevokedAt)
				require.Equal(t, "live-key", keyID)
			default:
				require.Nil(t, agents["active"].RevokedAt)
				require.Equal(t, uint64(7), agents["active"].Generation)
				require.Equal(t, "fixture-refresh", agents["active"].RefreshTokenHash)
				require.Less(t, agents["active"].SecurityEpoch, layout.Pointer.Epoch)
				require.Equal(t, "backup-key", keyID)
			}
			var stored string
			require.NoError(t, db.QueryRow("SELECT sealed FROM control_plane_private_keys WHERE key_id = ?", keyID).Scan(&stored))
			plaintext, err := secureconfig.DecryptString(stored)
			require.NoError(t, err)
			require.Equal(t, "test signing material", plaintext)
		})
	}
}
