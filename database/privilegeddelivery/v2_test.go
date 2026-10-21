package privilegeddelivery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func fixtureV2(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "state.db")+"?_busy_timeout=5000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sql, err := db.DB()
	require.NoError(t, err)
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { sql.Close() })
	require.NoError(t, db.AutoMigrate(&models.Client{}, &models.ClientDeploymentProfile{}, &models.ClientPrivilegedState{}, &models.ClientPrivilegedRevision{}, &models.ClientPrivilegedState{}, &models.ClientHostOperation{}, &models.ClientPrivilegedReceipt{}))
	id := "node"
	require.NoError(t, db.Create(&models.Client{UUID: id, Name: id}).Error)
	require.NoError(t, db.Create(&models.ClientDeploymentProfile{Client: id, Config: `{"platform":"linux","runtime_identity":"service-account","remote_control_enabled":false}`}).Error)
	_, err = Negotiate(db, id, "custom.service", "boot-before", 2, []int32{1, 2, 3})
	require.NoError(t, err)
	return db, id
}
func saveConfirm(t *testing.T, db *gorm.DB, id string, s Settings) (Revision, Revision) {
	t.Helper()
	snap, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	saved, err := SaveWithDB(db, id, s, snap.State.DesiredRevision, "")
	require.NoError(t, err)
	require.Empty(t, saved.Nonce)
	approved, err := ConfirmWithDB(db, id, saved.Revision, "admin")
	require.NoError(t, err)
	return saved, approved
}
func observation(r Revision) *hostv1.DaemonObservation {
	cap := func(v bool) *reportv1.CapabilityState { return &reportv1.CapabilityState{Available: v} }
	return &hostv1.DaemonObservation{Revision: r.Revision, ConfigHash: r.ConfigHash, ApprovalGeneration: r.ApprovalGeneration, ServiceId: "custom.service", StartupId: "boot-after", PrivilegeMode: reportv1.PrivilegeMode(r.ToPrivilegeMode), ObservedAt: timestamppb.Now(), EffectiveCapabilities: &reportv1.AgentCapabilities{PrivilegeMode: reportv1.PrivilegeMode(r.ToPrivilegeMode), RemoteControl: cap(r.Settings.RemoteControlEnabled), Webssh: cap(r.Settings.RemoteControlEnabled && r.Settings.WebSSHEnabled), Execution: cap(r.Settings.RemoteControlEnabled && r.Settings.ExecutionEnabled)}}
}
func prepare(t *testing.T, db *gorm.DB, id string, r Revision) *hostv1.PrepareHostOperationRequest {
	t.Helper()
	request := &hostv1.PrepareHostOperationRequest{AgentId: id, OperationId: "operation", Revision: r.Revision, ApprovalGeneration: r.ApprovalGeneration, ConfigHash: r.ConfigHash, Nonce: r.Nonce}
	_, err := PrepareOperation(db, request)
	require.NoError(t, err)
	return request
}
func localSuccess(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	_, err := ReportOperation(db, &hostv1.ReportHostOperationRequest{AgentId: id, OperationId: "operation", ReceiptId: "host-success", Phase: hostv1.HostOperationPhase_HOST_OPERATION_PHASE_LOCAL_APPLIED, LocalAuthentication: "pam", Operator: "admin"})
	require.NoError(t, err)
}

func TestV2ReceiptNeverApprovesAndConfirmationIsIdempotent(t *testing.T) {
	db, id := fixtureV2(t)
	saved, err := SaveWithDB(db, id, Settings{RequiredPrivilegeMode: 1}, 0, "")
	require.NoError(t, err)
	got, err := ReportWithDB(db, id, saved.Revision, StateNeedsManualAuthorization, 1, "")
	require.NoError(t, err)
	require.Equal(t, StateNeedsConfirmation, got.State)
	require.Nil(t, got.ConfirmedAt)
	require.NotNil(t, got.ReceivedAt)
	approved, err := ConfirmWithDB(db, id, 1, "admin")
	require.NoError(t, err)
	require.NotEmpty(t, approved.Nonce)
	again, err := ConfirmWithDB(db, id, 1, "admin")
	require.NoError(t, err)
	require.Equal(t, approved.Nonce, again.Nonce)
	require.Equal(t, approved.ApprovalGeneration, again.ApprovalGeneration)
	require.Equal(t, approved.NonceExpires, again.NonceExpires)
	_, err = ReportWithDB(db, id, 1, StateDelivered, 1, "")
	require.Error(t, err)
}
func TestV2CASIncludesZeroAndConcurrentSavesHaveOneWinner(t *testing.T) {
	db, id := fixtureV2(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := SaveWithDB(db, id, Settings{}, 0, ""); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	require.Equal(t, 1, success)
	_, err := SaveWithDB(db, id, Settings{}, 0, "")
	require.Error(t, err)
}
func TestV2SupersededAndUnapprovedTasksCannotExecute(t *testing.T) {
	db, id := fixtureV2(t)
	_, approved := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 1})
	_, err := SaveWithDB(db, id, Settings{RequiredPrivilegeMode: 2}, 1, "")
	require.NoError(t, err)
	_, err = PrepareOperation(db, &hostv1.PrepareHostOperationRequest{AgentId: id, OperationId: "stale", Revision: 1, ApprovalGeneration: approved.ApprovalGeneration, ConfigHash: approved.ConfigHash, Nonce: approved.Nonce})
	require.Error(t, err)
	_, err = ConfirmWithDB(db, id, 1, "admin")
	require.Error(t, err)
	_, err = Observe(db, id, "unapproved", observation(Revision{Revision: 2, ToPrivilegeMode: 2}))
	require.Error(t, err)
}
func TestV2HostSuccessRequiresRestartedDaemonBeforeApplied(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RemoteControlEnabled: true, WebSSHEnabled: true, RequiredPrivilegeMode: 1})
	req := prepare(t, db, id, r)
	first, err := GetOperation(db, id, req.OperationId)
	require.NoError(t, err)
	again, err := PrepareOperation(db, req)
	require.NoError(t, err)
	require.True(t, proto.Equal(first, again))
	second := proto.Clone(req).(*hostv1.PrepareHostOperationRequest)
	second.OperationId = "replay"
	_, err = PrepareOperation(db, second)
	require.Error(t, err)
	o := observation(r)
	o.OperationId = "operation"
	_, err = Observe(db, id, "before-host", o)
	require.Error(t, err)
	localSuccess(t, db, id)
	snap, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Zero(t, snap.State.AppliedRevision)
	o.StartupId = "boot-before"
	_, err = Observe(db, id, "no-restart", o)
	require.Error(t, err)
	o.StartupId = "boot-after"
	o.PrivilegeMode = 2
	_, err = Observe(db, id, "wrong-identity", o)
	require.Error(t, err)
	o = observation(r)
	o.OperationId = "operation"
	applied, err := Observe(db, id, "daemon-applied", o)
	require.NoError(t, err)
	require.Equal(t, StateDelivered, applied.State)
	_, err = Observe(db, id, "daemon-applied", o)
	require.NoError(t, err)
	o.ConfigHash = "tamper"
	_, err = Observe(db, id, "daemon-applied", o)
	require.Error(t, err)
	allowed, err := clients.PrivilegedCapabilityAllowedWithDB(db, id, "webssh")
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, err = clients.PrivilegedCapabilityAllowedWithDB(db, id, "execution")
	require.NoError(t, err)
	require.False(t, allowed)
	for n := uint64(1); n < 8; n++ {
		_, err = SaveWithDB(db, id, Settings{RequiredPrivilegeMode: 1}, n, "")
		require.NoError(t, err)
	}
	snap, err = GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Equal(t, uint64(1), snap.State.AppliedRevision)
	require.NotNil(t, snap.Applied)
	allowed, err = clients.PrivilegedCapabilityAllowedWithDB(db, id, "webssh")
	require.NoError(t, err)
	require.False(t, allowed, "pending revocation blocks new sessions")
	_, err = ReportWithDB(db, id, 1, StateNeedsManualAuthorization, 2, "")
	require.NoError(t, err)
	snap, err = GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Equal(t, StateDelivered, snap.Applied.State)
}
func TestV2SameIdentityApprovalExpiresAndCanBeReissued(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 2})
	require.Equal(t, StateAwaitingAgent, r.State)
	expired := time.Now().Add(-time.Minute)
	require.NoError(t, db.Model(&models.ClientPrivilegedRevision{}).Where("client = ?", id).Update("nonce_expires", expired).Error)
	_, err := Observe(db, id, "expired", observation(r))
	require.Error(t, err)
	renewed, err := ConfirmWithDB(db, id, 1, "admin")
	require.NoError(t, err)
	require.Equal(t, r.ApprovalGeneration+1, renewed.ApprovalGeneration)
	require.NotEqual(t, r.Nonce, renewed.Nonce)
	_, err = Observe(db, id, "old-generation", observation(r))
	require.Error(t, err)
	_, err = Observe(db, id, "renewed", observation(renewed))
	require.NoError(t, err)
}
func TestV2LeaseExpiryFailureAndReceiptConflict(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 1})
	prepare(t, db, id, r)
	require.NoError(t, db.Model(&models.ClientHostOperation{}).Where("client = ?", id).Update("lease_expires_at", time.Now().Add(-time.Minute)).Error)
	_, err := ReportOperation(db, &hostv1.ReportHostOperationRequest{AgentId: id, OperationId: "operation", ReceiptId: "late", Phase: 3, Operator: "admin", LocalAuthentication: "pam"})
	require.Error(t, err)
	fail := &hostv1.ReportHostOperationRequest{AgentId: id, OperationId: "operation", ReceiptId: "failure", Phase: 4, Error: "service restored"}
	_, err = ReportOperation(db, fail)
	require.NoError(t, err)
	_, err = ReportOperation(db, fail)
	require.NoError(t, err)
	fail.Error = "different"
	_, err = ReportOperation(db, fail)
	require.Error(t, err)
	snap, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Zero(t, snap.State.AppliedRevision)
	require.Equal(t, StateFailed, snap.Desired.State)
}
func TestV2UnnegotiatedAndMutuallyExclusiveSettings(t *testing.T) {
	db, id := fixtureV2(t)
	require.NoError(t, db.Model(&models.ClientPrivilegedState{}).Where("client = ?", id).Update("protocol_version", 1).Error)
	r, err := SaveWithDB(db, id, Settings{}, 0, "")
	require.NoError(t, err)
	_, err = ConfirmWithDB(db, id, r.Revision, "admin")
	require.Error(t, err)
	_, err = SaveWithDB(db, id, Settings{RemoteControlEnabled: true, RescueHelperEnabled: true}, 1, "")
	require.Error(t, err)
}
func TestV2HashVectors(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "privileged-settings-v1.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var vectors []struct {
		Booleans []int  `json:"booleans"`
		Mode     int32  `json:"required_privilege_mode"`
		Hash     string `json:"sha256"`
	}
	require.NoError(t, json.Unmarshal(data, &vectors))
	for _, v := range vectors {
		require.Equal(t, v.Hash, settingsHash(Settings{RemoteControlEnabled: v.Booleans[0] != 0, WebSSHEnabled: v.Booleans[1] != 0, ExecutionEnabled: v.Booleans[2] != 0, EnableGPU: v.Booleans[3] != 0, RescueHelperEnabled: v.Booleans[4] != 0, RequiredPrivilegeMode: v.Mode}))
	}
}

func TestV2HelperInstallDoesNotPromoteOrdinaryAgent(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RescueHelperEnabled: true, RequiredPrivilegeMode: 2})
	auth, err := Authorization(r)
	require.NoError(t, err)
	require.Equal(t, []hostv1.HostAction{hostv1.HostAction_HOST_ACTION_INSTALL_RESCUE_HELPER}, auth.Actions)
	require.Equal(t, int32(2), r.ToPrivilegeMode)
	prepare(t, db, id, r)
	localSuccess(t, db, id)
	o := observation(r)
	o.OperationId = "operation"
	o.HelperInstalled = true
	o.HelperRunning = true
	o.HelperAuthorized = true
	o.EffectiveCapabilities.RescueHelper = &reportv1.CapabilityState{Available: true}
	_, err = Observe(db, id, "helper-ack", o)
	require.NoError(t, err)
	profile, _, err := clients.GetDeploymentProfileWithDB(db, id)
	require.NoError(t, err)
	require.True(t, profile.RescueEnabled)
	require.Equal(t, clients.AgentRuntimeIdentityServiceAccount, profile.RuntimeIdentity)
	allowed, err := clients.PrivilegedCapabilityAllowedWithDB(db, id, "rescue")
	require.NoError(t, err)
	require.True(t, allowed)
	_, err = SaveWithDB(db, id, Settings{RequiredPrivilegeMode: 2}, 1, "")
	require.NoError(t, err)
	approved, err := ConfirmWithDB(db, id, 2, "admin")
	require.NoError(t, err)
	auth, err = Authorization(approved)
	require.NoError(t, err)
	require.Equal(t, []hostv1.HostAction{hostv1.HostAction_HOST_ACTION_REMOVE_RESCUE_HELPER}, auth.Actions)
	allowed, err = clients.PrivilegedCapabilityAllowedWithDB(db, id, "rescue")
	require.NoError(t, err)
	require.False(t, allowed)
}
func TestV2ObservationRejectsTamperingAndDisabledCapabilities(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 2})
	mutations := map[string]func(*hostv1.DaemonObservation){
		"hash":       func(o *hostv1.DaemonObservation) { o.ConfigHash = "wrong" },
		"generation": func(o *hostv1.DaemonObservation) { o.ApprovalGeneration++ },
		"service":    func(o *hostv1.DaemonObservation) { o.ServiceId = "other.service" },
		"mode":       func(o *hostv1.DaemonObservation) { o.PrivilegeMode = 1 },
		"execution": func(o *hostv1.DaemonObservation) {
			o.EffectiveCapabilities.Execution = &reportv1.CapabilityState{Available: true}
		},
		"GPU": func(o *hostv1.DaemonObservation) {
			o.EffectiveCapabilities.Gpu = &reportv1.CapabilityState{Available: true}
		},
		"helper": func(o *hostv1.DaemonObservation) { o.HelperInstalled = true },
		"future": func(o *hostv1.DaemonObservation) { o.ObservedAt = timestamppb.New(time.Now().Add(time.Hour)) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			o := observation(r)
			mutate(o)
			_, err := Observe(db, id, "bad-"+name, o)
			require.Error(t, err)
			snap, err := GetStateWithDB(db, id)
			require.NoError(t, err)
			require.Zero(t, snap.State.AppliedRevision)
		})
	}
	_, err := Observe(db, id, "", observation(r))
	require.Error(t, err)
}
func TestV2MigrationRetainsLegacyHistoryWithoutInventingApproval(t *testing.T) {
	db, id := fixtureV2(t)
	require.NoError(t, db.Migrator().DropTable(&models.ClientPrivilegedRevision{}, &models.ClientPrivilegedState{}, &models.ClientHostOperation{}, &models.ClientPrivilegedReceipt{}))
	// Old shape intentionally has no v2 columns. Upgrade the actual SQLite schema.
	type legacyRevision struct {
		Client      string `gorm:"primaryKey"`
		Revision    uint64 `gorm:"primaryKey"`
		Config      string
		State       int32
		Nonce       string
		ConfirmedAt *time.Time
	}
	require.NoError(t, db.Table("client_privileged_revisions").AutoMigrate(&legacyRevision{}))
	require.NoError(t, db.Table("client_privileged_revisions").Create(&legacyRevision{Client: id, Revision: 1, Config: `{"remote_control_enabled":false}`, State: 2, Nonce: "old-fixture"}).Error)
	require.NoError(t, db.AutoMigrate(&models.ClientPrivilegedRevision{}, &models.ClientPrivilegedState{}, &models.ClientHostOperation{}, &models.ClientPrivilegedReceipt{}))
	snap, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Equal(t, uint64(1), snap.State.DesiredRevision)
	require.Zero(t, snap.State.AppliedRevision)
	require.Nil(t, snap.Desired.ConfirmedAt)
	require.Equal(t, uint32(1), snap.Desired.ProtocolVersion)
	_, err = ConfirmWithDB(db, id, 1, "admin")
	require.Error(t, err)
	next, err := SaveWithDB(db, id, Settings{}, 1, "")
	require.NoError(t, err)
	require.Equal(t, uint64(2), next.Revision)
	var old models.ClientPrivilegedRevision
	require.NoError(t, db.First(&old, "client = ? AND revision = ?", id, 1).Error)
	require.Nil(t, old.ConfirmedAt)
	require.Equal(t, StateSuperseded, old.State)
	require.Equal(t, `{"remote_control_enabled":false}`, old.Config)
}
func TestV2SuccessfulHostOperationAllowsDelayedDaemonReceipt(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 1})
	prepare(t, db, id, r)
	localSuccess(t, db, id)
	require.NoError(t, db.Model(&models.ClientPrivilegedRevision{}).Where("client = ?", id).Update("nonce_expires", time.Now().Add(-time.Minute)).Error)
	o := observation(r)
	o.OperationId = "operation"
	_, err := Observe(db, id, "delayed-daemon", o)
	require.NoError(t, err)
}

func TestV2SameIdentityRemoteGrantDoesNotRequireHostOperation(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RemoteControlEnabled: true, WebSSHEnabled: true, RequiredPrivilegeMode: 2})
	require.Equal(t, StateAwaitingAgent, r.State)
	auth, err := Authorization(r)
	require.NoError(t, err)
	require.Empty(t, auth.Actions)
	_, err = Observe(db, id, "same-identity", observation(r))
	require.NoError(t, err)
	profile, _, err := clients.GetDeploymentProfileWithDB(db, id)
	require.NoError(t, err)
	require.True(t, profile.RemoteControlEnabled)
	require.Equal(t, clients.AgentRuntimeIdentityServiceAccount, profile.RuntimeIdentity)
	allowed, err := clients.PrivilegedCapabilityAllowedWithDB(db, id, "webssh")
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, err = clients.PrivilegedCapabilityAllowedWithDB(db, id, "execution")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestV2FailureDoesNotInventAppliedAndCannotRegressSuccess(t *testing.T) {
	db, id := fixtureV2(t)
	_, r := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 2})
	_, err := FailApply(db, id, r.Revision, "wrong-failure", "wrong", r.ApprovalGeneration, "local policy blocked")
	require.Error(t, err)
	failed, err := FailApply(db, id, r.Revision, "failure", r.ConfigHash, r.ApprovalGeneration, "local policy blocked")
	require.NoError(t, err)
	require.Equal(t, StateFailed, failed.State)
	_, err = FailApply(db, id, r.Revision, "failure", r.ConfigHash, r.ApprovalGeneration, "local policy blocked")
	require.NoError(t, err)
	snap, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Zero(t, snap.State.AppliedRevision)
	_, err = Observe(db, id, "late-success", observation(r))
	require.Error(t, err)
	_, next := saveConfirm(t, db, id, Settings{RequiredPrivilegeMode: 2})
	_, err = Observe(db, id, "next-applied", observation(next))
	require.NoError(t, err)
	_, err = FailApply(db, id, next.Revision, "late-failure", next.ConfigHash, next.ApprovalGeneration, "stale error")
	require.Error(t, err)
	snap, err = GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Equal(t, next.Revision, snap.State.AppliedRevision)
}

func TestV2UnspecifiedIdentityIsResolvedBeforeHashAndApproval(t *testing.T) {
	db, id := fixtureV2(t)
	saved, approved := saveConfirm(t, db, id, Settings{RemoteControlEnabled: true, WebSSHEnabled: true})
	require.Equal(t, int32(2), saved.Settings.RequiredPrivilegeMode)
	require.Equal(t, settingsHash(saved.Settings), saved.ConfigHash)
	require.Equal(t, saved.ConfigHash, approved.ConfigHash)
	require.Equal(t, reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_NON_ROOT, reportv1.PrivilegeMode(approved.ToPrivilegeMode))
	require.Equal(t, StateAwaitingAgent, approved.State)
	_, err := Observe(db, id, "explicit-same-identity", observation(approved))
	require.NoError(t, err)
}
