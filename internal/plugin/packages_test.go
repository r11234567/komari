package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func stageReviewFixture(t *testing.T, version, script string) PackageReview {
	t.Helper()
	path := writePluginZip(t, map[string]string{
		"komari-plugin.json": fmt.Sprintf(`{"short":"review","name":"Review fixture","version":%q,"permissions":{"allowRoutes":true,"allowHTMLInject":true,"timeout":5}}`, version),
		"script.js":          script,
	})
	_, err := StageZip(path, "Synthetic upload", "review", version)
	require.NoError(t, err)
	digest, err := digestFile(path)
	require.NoError(t, err)
	rows, err := ListPackages()
	require.NoError(t, err)
	for _, row := range rows {
		if row.Digest == digest {
			return row
		}
	}
	t.Fatal("staged package missing")
	return PackageReview{}
}
func reviewScript(value string) string {
	return fmt.Sprintf(`const server=require("server"); function load(){server.route("GET","/review-fixture",(req,res)=>res.end(%q));server.injectHTML("","<span>approved fixture</span>");}`, value)
}
func decideFixture(t *testing.T, p PackageReview, action string) {
	t.Helper()
	require.NoError(t, DecidePackage("review", p.Digest, receiptDigest("review"), action, "synthetic-human", func(actual PackageReview) error {
		require.Equal(t, p.Digest, actual.Digest)
		require.Equal(t, p.PermissionsHash, actual.PermissionsHash)
		require.Equal(t, p.Plugin.Version, actual.Plugin.Version)
		return nil
	}))
}
func assertReviewRoute(t *testing.T, engine *gin.Engine, status int, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest("GET", "/review-fixture", nil))
	require.Equal(t, status, rec.Code, rec.Body.String())
	if body != "" {
		require.Equal(t, body, rec.Body.String())
	}
}

func TestPackageAdmissionKeepsOldVersionAndRequiresNewApprovalForSamePermissions(t *testing.T) {
	withTempDataDir(t)
	engine := gin.New()
	Init(engine)
	old := stageReviewFixture(t, "1.0.0", reviewScript("old"))
	require.Empty(t, List())
	require.NoError(t, LoadAll())
	assertReviewRoute(t, engine, 404, "")
	decideFixture(t, old, "activate")
	assertReviewRoute(t, engine, 200, "old")
	require.NoError(t, os.WriteFile(filepath.Join(StorageDir, "review", "keep.txt"), []byte("persistent"), 0600))
	require.NoError(t, SaveConfiguration("review", map[string]any{"kept": "configuration"}))
	// Same version and permissions, different code: approval cannot carry over.
	next := stageReviewFixture(t, "1.0.0", reviewScript("new"))
	require.NotEqual(t, old.Digest, next.Digest)
	require.Equal(t, old.PermissionsHash, next.PermissionsHash)
	assertReviewRoute(t, engine, 200, "old")
	denied := errors.New("fresh 2FA missing")
	require.ErrorIs(t, DecidePackage("review", next.Digest, old.Digest, "activate", "human", func(PackageReview) error { return denied }), denied)
	assertReviewRoute(t, engine, 200, "old")
	decideFixture(t, next, "activate")
	assertReviewRoute(t, engine, 200, "new")
	data, err := os.ReadFile(filepath.Join(StorageDir, "review", "keep.txt"))
	require.NoError(t, err)
	require.Equal(t, "persistent", string(data))
	config, err := GetConfiguration("review")
	require.NoError(t, err)
	require.Equal(t, "configuration", config["kept"])
	// Rollback is another explicit approval of exact retained bytes.
	decideFixture(t, old, "activate")
	assertReviewRoute(t, engine, 200, "old")
	decideFixture(t, old, "revoke")
	assertReviewRoute(t, engine, 404, "")
	require.NoError(t, LoadAll())
	require.ErrorIs(t, SetEnabled("review", true, false), ErrPermissionApprovalRequired)
	assertReviewRoute(t, engine, 404, "")
	rows, err := ListPackages()
	require.NoError(t, err)
	for _, row := range rows {
		if row.Digest == old.Digest {
			require.True(t, row.Revoked)
			require.Equal(t, "revoked", row.Status)
		}
	}
}

func TestPackageFailedActivationRestoresApprovedRuntimeAndData(t *testing.T) {
	withTempDataDir(t)
	engine := gin.New()
	Init(engine)
	old := stageReviewFixture(t, "1", reviewScript("old"))
	decideFixture(t, old, "activate")
	bad := stageReviewFixture(t, "2", `throw new Error("synthetic startup failure");`)
	err := DecidePackage("review", bad.Digest, old.Digest, "activate", "human", func(PackageReview) error { return nil })
	require.Error(t, err)
	require.Contains(t, err.Error(), "synthetic startup failure")
	assertReviewRoute(t, engine, 200, "old")
	require.Equal(t, old.Digest, receiptDigest("review"))
	require.True(t, global.stateStore().get("review").Enabled)
	_, err = os.Stat(journalPath("review"))
	require.True(t, os.IsNotExist(err))
}

func TestPackageTamperingAndStaleRevisionNeverConsumeProofOrExecute(t *testing.T) {
	withTempDataDir(t)
	engine := gin.New()
	Init(engine)
	old := stageReviewFixture(t, "1", reviewScript("old"))
	decideFixture(t, old, "activate")
	next := stageReviewFixture(t, "2", reviewScript("new"))
	verified := false
	verify := func(PackageReview) error { verified = true; return nil }
	require.Error(t, DecidePackage("review", next.Digest, "stale", "activate", "human", verify))
	require.False(t, verified)
	f, err := os.OpenFile(packagePath(next.Digest, ".zip"), os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.Write([]byte("changed"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Error(t, DecidePackage("review", next.Digest, old.Digest, "activate", "human", verify))
	require.False(t, verified)
	assertReviewRoute(t, engine, 200, "old")
}

func TestPackageRevocationStillWorksWhenRetainedArchiveIsMissing(t *testing.T) {
	withTempDataDir(t)
	engine := gin.New()
	Init(engine)
	p := stageReviewFixture(t, "1", reviewScript("old"))
	decideFixture(t, p, "activate")
	require.NoError(t, os.Remove(packagePath(p.Digest, ".zip")))
	decideFixture(t, p, "revoke")
	assertReviewRoute(t, engine, 404, "")
	require.Empty(t, global.stateStore().get("review").ApprovedPackageDigest)
}

func TestPackageRecoveryRestoresOldRevisionBeforeStartupLoadsAnyCode(t *testing.T) {
	withTempDataDir(t)
	engine := gin.New()
	Init(engine)
	old := stageReviewFixture(t, "1", reviewScript("old"))
	decideFixture(t, old, "activate")
	next := stageReviewFixture(t, "2", reviewScript("new"))
	oldState := global.stateStore().get("review")
	_, prepared, err := unpackPackage(packagePath(next.Digest, ".zip"))
	require.NoError(t, err)
	require.NoError(t, writeReceipt(prepared, next.Digest))
	backup, err := os.MkdirTemp(DataDir, ".rollback-")
	require.NoError(t, err)
	require.NoError(t, os.Remove(backup))
	j := packageJournal{Short: "review", Prepared: filepath.Base(prepared), Backup: filepath.Base(backup), Digest: next.Digest, Old: oldState, HadOld: true}
	b, err := json.Marshal(j)
	require.NoError(t, err)
	require.NoError(t, atomicPackageFile(journalPath("review"), b))
	require.NoError(t, global.unload("review"))
	require.NoError(t, os.Rename(filepath.Join(DataDir, "review"), backup))
	require.NoError(t, os.Rename(prepared, filepath.Join(DataDir, "review")))
	partial := oldState
	partial.ApprovedPackageDigest = next.Digest
	require.NoError(t, global.stateStore().set("review", partial))
	require.NoError(t, LoadAll())
	require.Equal(t, old.Digest, receiptDigest("review"))
	assertReviewRoute(t, engine, 200, "old")
}

func TestPackageApprovalStateWriteFailureFailsClosed(t *testing.T) {
	withTempDataDir(t)
	engine := gin.New()
	Init(engine)
	next := stageReviewFixture(t, "1", reviewScript("must not run"))
	require.NoError(t, os.Mkdir(filepath.Join(DataDir, stateFileName), 0700))
	require.Error(t, DecidePackage("review", next.Digest, "", "activate", "human", func(PackageReview) error { return nil }))
	assertReviewRoute(t, engine, 404, "")
	require.False(t, global.stateStore().get("review").Enabled)
}

func TestPackageCatalogMismatchAndReservedReceiptRejectedBeforeInstallation(t *testing.T) {
	withTempDataDir(t)
	path := writePluginZip(t, map[string]string{"komari-plugin.json": `{"name":"test","short":"review","version":"1"}`, "script.js": "function load(){}"})
	_, err := StageZip(path, "catalog", "different", "1")
	require.Error(t, err)
	require.Empty(t, List())
	reserved := writePluginZip(t, map[string]string{"komari-plugin.json": `{"name":"test","short":"review","version":"1"}`, "script.js": "function load(){}", packageReceipt: `{"digest":"forged"}`})
	_, err = StageZip(reserved, "upload", "", "")
	require.Error(t, err)
	rows, err := ListPackages()
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestPackageLegacyPermissionOnlyStateCannotAutoStart(t *testing.T) {
	withTempDataDir(t)
	Init(gin.New())
	path := writePluginZip(t, map[string]string{"komari-plugin.json": `{"name":"test","short":"review","version":"1"}`, "script.js": "function load(){}"})
	_, err := InstallZip(path)
	require.NoError(t, err)
	require.NoError(t, global.stateStore().set("review", PluginState{Enabled: true}))
	require.Error(t, LoadAll())
	require.Nil(t, global.instanceFor("review"))
	rows, err := ListPackages()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	again, err := ListPackages()
	require.NoError(t, err)
	require.Len(t, again, 1)
	require.Equal(t, rows[0].Digest, again[0].Digest)
	decideFixture(t, rows[0], "activate")
	require.NotNil(t, global.instanceFor("review"))
}
