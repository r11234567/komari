package privilegeddelivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
	"time"
)

type BaselineResult struct {
	Initialized   bool
	Runtime       *configv1.DesiredConfig
	Privileged    Revision
	SkippedReason string
}

// InitializeBaseline atomically initializes both tracks on eligible new nodes.
// Existing nodes can only add a missing deny-only privileged baseline while
// preserving ordinary history. Neither path replaces privileged history.
func InitializeBaseline(db *gorm.DB, r *configv1.InitializeNodeConfigRequest, profile clients.DeploymentProfile) (BaselineResult, error) {
	var out BaselineResult
	if r.Runtime == nil || r.Privileged == nil || len(r.PolicyHash) != 64 {
		return out, errors.New("runtime, privileged settings and policy hash are required")
	}
	request, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(request)
	hash := hex.EncodeToString(sum[:])
	err = db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, r.AgentId)
		if err != nil {
			return err
		}
		var old models.ClientConfigBaseline
		if e := tx.First(&old, "client = ?", r.AgentId).Error; e == nil {
			if old.RequestHash != hash {
				return errors.New("installation baseline retry changed its immutable request")
			}
			if len(old.Runtime) > 0 {
				out.Runtime = &configv1.DesiredConfig{}
				if e = proto.Unmarshal(old.Runtime, out.Runtime); e != nil {
					return e
				}
			}
			row, e := lockRevision(tx, r.AgentId, 1)
			if e != nil {
				return e
			}
			out.Privileged = toRevision(row)
			out.Initialized = true
			return nil
		} else if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		var client models.Client
		if err = tx.First(&client, "uuid = ?", r.AgentId).Error; err != nil {
			return err
		}
		if !client.ConfigBaselineEligible && !r.PreserveRuntime {
			out.SkippedReason = "existing node requires preserve_runtime for a deny-only privileged baseline"
			return nil
		}
		var history int64
		if err = tx.Model(&models.ClientPrivilegedRevision{}).Where("client = ?", r.AgentId).Count(&history).Error; err != nil {
			return err
		}
		var existing models.ClientDeploymentProfile
		e := tx.First(&existing, "client = ?", r.AgentId).Error
		if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		if history != 0 || state.DesiredRevision != 0 || state.AppliedRevision != 0 {
			out.SkippedReason = "privileged configuration history already exists"
			return nil
		}
		if !r.PreserveRuntime && (existing.Revision != 0 || existing.AppliedRevision != 0) {
			out.SkippedReason = "ordinary configuration history must be preserved"
			return nil
		}
		if state.ProtocolVersion != 2 || state.ServiceID != r.ServiceId || state.PrivilegeMode != int32(r.Privileged.RequiredPrivilegeMode) {
			return errors.New("baseline does not match negotiated service identity")
		}
		if r.Privileged.RescueHelperEnabled != nil && r.Privileged.GetRescueHelperEnabled() {
			return errors.New("first-install baseline cannot install a rescue helper")
		}
		s := Settings{RemoteControlEnabled: r.Privileged.GetRemoteControlEnabled(), WebSSHEnabled: r.Privileged.GetWebsshEnabled(), ExecutionEnabled: r.Privileged.GetExecutionEnabled(), EnableGPU: r.Privileged.GetEnableGpu(), RequiredPrivilegeMode: int32(r.Privileged.RequiredPrivilegeMode)}
		if r.PreserveRuntime && (s.RemoteControlEnabled || s.WebSSHEnabled || s.ExecutionEnabled || s.EnableGPU) {
			return errors.New("migration baseline must deny all optional privileged capabilities")
		}
		if !s.RemoteControlEnabled && (s.WebSSHEnabled || s.ExecutionEnabled) {
			return errors.New("baseline child capability requires master authorization")
		}
		now := time.Now().UTC()
		configHash := settingsHash(s)
		authorization, err := proto.Marshal(&hostv1.HostAuthorization{AgentId: r.AgentId, Revision: 1, ConfigHash: configHash, ApprovalGeneration: 1, ServiceId: r.ServiceId, TargetPrivilegeMode: reportv1.PrivilegeMode(state.PrivilegeMode), IssuedAt: timestamppb.New(now), ApprovedBy: "local-installation"})
		if err != nil {
			return err
		}
		row := models.ClientPrivilegedRevision{Client: r.AgentId, Revision: 1, Config: encodeSettings(s), ProtocolVersion: 2, ConfigHash: configHash, ApprovalGeneration: 1, Authorization: authorization, ConfirmedBy: "local-installation", ConfirmedAt: &now, InstallationBaseline: true, State: StateAwaitingAgent, UpgradeClass: ClassManualConfirm, FromPrivilegeMode: state.PrivilegeMode, ToPrivilegeMode: state.PrivilegeMode, SavedAt: now, CreatedAt: now}
		state.DesiredRevision = 1
		if err = advance(tx, &state, &row); err != nil {
			return err
		}
		if r.PreserveRuntime {
			// Never rewrite the ordinary profile, its revisions, ACKs or timestamps.
			if err = tx.Create(&models.ClientConfigBaseline{Client: r.AgentId, RequestHash: hash, Runtime: []byte{}, CreatedAt: now}).Error; err != nil {
				return err
			}
			if err = tx.Model(&client).Update("config_baseline_eligible", false).Error; err != nil {
				return err
			}
			out = BaselineResult{Initialized: true, Privileged: toRevision(row)}
			return nil
		}
		profile.PrivilegedDeliveryVersion = 2
		profile.ServiceName = r.ServiceId
		profile.EnableGPU = s.EnableGPU
		profile.RemoteControlEnabled = s.RemoteControlEnabled
		if state.PrivilegeMode == 1 || state.PrivilegeMode == 3 {
			profile.RuntimeIdentity = clients.AgentRuntimeIdentityPrivileged
		}

		encoded, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		deployment := models.ClientDeploymentProfile{Client: r.AgentId, Config: string(encoded), Revision: 1, AppliedRevision: 0, DeliveryStatus: clients.DeploymentDeliverySaved, SavedAt: &now, DeliveryUpdatedAt: &now, CreatedAt: now}
		if e == nil {
			if err = tx.Save(&deployment).Error; err != nil {
				return err
			}
		} else if err = tx.Create(&deployment).Error; err != nil {
			return err
		}
		desired := &configv1.DesiredConfig{AgentId: r.AgentId, Revision: 1, Runtime: r.Runtime, SavedAt: timestamppb.New(now)}
		runtime, err := proto.Marshal(desired)
		if err != nil {
			return err
		}
		if err = tx.Create(&models.ClientConfigBaseline{Client: r.AgentId, RequestHash: hash, Runtime: runtime, CreatedAt: now}).Error; err != nil {
			return err
		}
		if err = tx.Model(&client).Update("config_baseline_eligible", false).Error; err != nil {
			return err
		}
		resetDay := 0
		if profile.EnableMonthRotate {
			resetDay = profile.MonthRotate
		}
		if err = tx.Model(&client).Update("traffic_reset_day", resetDay).Error; err != nil {
			return err
		}
		out = BaselineResult{Initialized: true, Runtime: desired, Privileged: toRevision(row)}
		return nil
	})
	return out, err
}
