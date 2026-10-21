package privilegeddelivery

import (
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
	"strings"
	"testing"
	"time"
)

func baselineFixture(t *testing.T) (*gorm.DB, string, *configv1.InitializeNodeConfigRequest) {
	db, id := fixtureV2(t)
	require.NoError(t, db.AutoMigrate(&models.ClientConfigBaseline{}))
	require.NoError(t, db.Model(&models.Client{}).Where("uuid = ?", id).Update("config_baseline_eligible", true).Error)
	r := &configv1.InitializeNodeConfigRequest{AgentId: id, ServiceId: "custom.service", PolicyHash: strings.Repeat("a", 64), Runtime: &configv1.RuntimeConfig{}, Privileged: &configv1.PrivilegedConfig{RequiredPrivilegeMode: reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_NON_ROOT}}
	return db, id, r
}
func TestInitialBaselinesAreIdempotentAndAwaitDaemonACK(t *testing.T) {
	db, id, r := baselineFixture(t)
	first, err := InitializeBaseline(db, r, clients.DefaultDeploymentProfile(models.Client{}))
	require.NoError(t, err)
	require.True(t, first.Initialized)
	require.Equal(t, uint64(1), first.Runtime.Revision)
	require.Equal(t, uint64(1), first.Privileged.Revision)
	require.True(t, first.Privileged.InstallationBaseline)
	again, err := InitializeBaseline(db, r, clients.DefaultDeploymentProfile(models.Client{}))
	require.NoError(t, err)
	require.Equal(t, first.Privileged.Revision, again.Privileged.Revision)
	var count int64
	require.NoError(t, db.Model(&models.ClientPrivilegedRevision{}).Where("client = ?", id).Count(&count).Error)
	require.EqualValues(t, 1, count)
	state, err := GetStateWithDB(db, id)
	require.NoError(t, err)
	require.Zero(t, state.State.AppliedRevision)
	var runtime models.ClientDeploymentProfile
	require.NoError(t, db.First(&runtime, "client = ?", id).Error)
	require.EqualValues(t, 1, runtime.Revision)
	require.Zero(t, runtime.AppliedRevision)
	o := &hostv1.DaemonObservation{Revision: 1, ConfigHash: first.Privileged.ConfigHash, ApprovalGeneration: 1, ServiceId: r.ServiceId, StartupId: "boot-before", PrivilegeMode: reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_NON_ROOT, ObservedAt: timestamppb.New(time.Now().Add(time.Second)), EffectiveCapabilities: &reportv1.AgentCapabilities{PrivilegeMode: reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_NON_ROOT}}
	_, err = Observe(db, id, "baseline-ack", o)
	require.NoError(t, err)
	_, err = Observe(db, id, "baseline-ack", o)
	require.NoError(t, err)
	state, err = GetStateWithDB(db, id)
	require.NoError(t, err)
	require.EqualValues(t, 1, state.State.AppliedRevision)
	r.PolicyHash = strings.Repeat("b", 64)
	_, err = InitializeBaseline(db, r, clients.DefaultDeploymentProfile(models.Client{}))
	require.Error(t, err)
}
func TestBaselineCannotResetOldNodeOrGrantNewIdentity(t *testing.T) {
	for _, which := range []string{"old-node", "runtime-r4", "privileged-history", "identity", "helper"} {
		t.Run(which, func(t *testing.T) {
			db, id, r := baselineFixture(t)
			switch which {
			case "old-node":
				require.NoError(t, db.Model(&models.Client{}).Where("uuid = ?", id).Update("config_baseline_eligible", false).Error)
			case "runtime-r4":
				require.NoError(t, db.Model(&models.ClientDeploymentProfile{}).Where("client = ?", id).Updates(map[string]any{"revision": 4, "applied_revision": 4}).Error)
			case "privileged-history":
				_, err := SaveWithDB(db, id, Settings{}, 0, "")
				require.NoError(t, err)
			case "identity":
				r.Privileged.RequiredPrivilegeMode = reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_ROOT
			case "helper":
				yes := true
				r.Privileged.RescueHelperEnabled = &yes
			}
			result, err := InitializeBaseline(db, r, clients.DefaultDeploymentProfile(models.Client{}))
			if which == "identity" || which == "helper" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.False(t, result.Initialized)
		})
	}
}
