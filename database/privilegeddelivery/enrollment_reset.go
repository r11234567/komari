package privilegeddelivery

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ResetForEnrollment runs in the transaction that replaces the node's credentials.
func ResetForEnrollment(tx *gorm.DB, id string, now time.Time) error {
	state, err := lockState(tx, id)
	if err != nil {
		return err
	}
	if err := supersedePending(tx, state, now); err != nil {
		return err
	}
	row := models.ClientPrivilegedRevision{
		Client: id, Revision: state.DesiredRevision + 1, EnrollmentReset: true,
		Config: encodeSettings(Settings{}), ConfigHash: settingsHash(Settings{}),
		ProtocolVersion: 2, State: StateUnspecified,
		Reasons: encodeReasons([]string{"privileged settings reset after Agent enrollment"}),
		SavedAt: now, CreatedAt: now,
	}
	state = models.ClientPrivilegedState{
		Client: id, DesiredRevision: row.Revision, UpdateSequence: state.UpdateSequence,
		ProtocolVersion: 2,
	}
	if err := advance(tx, &state, &row); err != nil {
		return err
	}
	// An r1 bootstrap receipt belongs to the prior installation.
	if err := tx.Delete(&models.ClientConfigBaseline{}, "client = ?", id).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.Client{}).Where("uuid = ?", id).Update("config_baseline_eligible", false).Error; err != nil {
		return err
	}
	if err := resetDeploymentProfile(tx, id, now); err != nil {
		return err
	}
	if err := tx.Delete(&models.ClientRescueHelper{}, "client = ?", id).Error; err != nil {
		return err
	}
	// A task authorized for the former installation must never reach the new one.
	return tx.Model(&models.RescueSession{}).
		Where("client = ? AND state IN ?", id, []commonv1.OperationState{
			commonv1.OperationState_OPERATION_STATE_QUEUED,
			commonv1.OperationState_OPERATION_STATE_RUNNING,
			commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED,
		}).
		Updates(map[string]any{
			"state":      commonv1.OperationState_OPERATION_STATE_CANCELLED,
			"error_code": "AGENT_REENROLLED", "error_message": "Agent re-enrolled",
			"finished_at": now, "updated_at": now,
		}).Error
}

func resetDeploymentProfile(tx *gorm.DB, id string, now time.Time) error {
	var row models.ClientDeploymentProfile
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "client = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	var config clients.DeploymentProfile
	if err := json.Unmarshal([]byte(row.Config), &config); err != nil {
		return err
	}
	config.PrivilegedDeliveryVersion = 0
	config.RuntimeIdentity = clients.AgentRuntimeIdentityServiceAccount
	config.EnableCustomServiceName = false
	config.ServiceName = ""
	config.RemoteControlEnabled = false
	config.DisableWebSSH = false
	config.EnableGPU = false
	config.RescueEnabled = false
	encoded, err := json.Marshal(config)
	if err != nil {
		return err
	}
	row.Config = string(encoded)
	// The same runtime values need a new receipt from the new installation.
	if row.Revision != 0 {
		row.Revision++
		row.AppliedRevision = 0
		row.DeliveryStatus = clients.DeploymentDeliverySaved
		row.DeliveryError = ""
		row.SentAt = nil
		row.FinishedAt = nil
		row.SavedAt = &now
		row.DeliveryUpdatedAt = &now
	}
	return tx.Save(&row).Error
}

func approveEnrollmentReset(tx *gorm.DB, state *models.ClientPrivilegedState, row *models.ClientPrivilegedRevision, now time.Time) error {
	settings := Settings{RequiredPrivilegeMode: state.PrivilegeMode}
	row.Config = encodeSettings(settings)
	row.ConfigHash = settingsHash(settings)
	row.FromPrivilegeMode = state.PrivilegeMode
	row.ToPrivilegeMode = state.PrivilegeMode
	row.ErrorDetail = ""
	row.FinishedAt = nil
	return approveRevision(tx, state, row, "agent-enrollment", nil, now)
}
