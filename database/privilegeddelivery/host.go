package privilegeddelivery

import (
	"errors"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
)

func Authorization(row Revision) (*hostv1.HostAuthorization, error) {
	if len(row.Authorization) == 0 {
		return nil, nil
	}
	var a hostv1.HostAuthorization
	err := proto.Unmarshal(row.Authorization, &a)
	return &a, err
}
func operationMessage(row models.ClientHostOperation) (*hostv1.HostOperation, error) {
	var a hostv1.HostAuthorization
	if err := proto.Unmarshal(row.Authorization, &a); err != nil {
		return nil, err
	}
	return &hostv1.HostOperation{OperationId: row.OperationID, Authorization: &a, Phase: hostv1.HostOperationPhase(row.Phase), LeaseExpiresAt: timestamppb.New(row.LeaseExpiresAt), Error: row.Error}, nil
}
func GetOperation(db *gorm.DB, id, key string) (*hostv1.HostOperation, error) {
	var op models.ClientHostOperation
	if err := db.First(&op, "client = ? AND operation_id = ?", id, key).Error; err != nil {
		return nil, err
	}
	return operationMessage(op)
}

func PrepareOperation(db *gorm.DB, r *hostv1.PrepareHostOperationRequest) (*hostv1.HostOperation, error) {
	var result *hostv1.HostOperation
	if strings.TrimSpace(r.OperationId) == "" || len(r.OperationId) > 128 {
		return nil, errors.New("operation ID is required (max 128 bytes)")
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, r.AgentId)
		if err != nil {
			return err
		}
		row, err := lockRevision(tx, r.AgentId, r.Revision)
		if err != nil {
			return err
		}
		if err := validateCurrent(state, row); err != nil {
			return err
		}
		if r.ApprovalGeneration != row.ApprovalGeneration || r.ConfigHash != row.ConfigHash || r.Nonce == "" || r.Nonce != row.Nonce {
			return errors.New("approval generation, hash or nonce mismatch")
		}
		var existing models.ClientHostOperation
		err = tx.First(&existing, "client = ? AND operation_id = ?", r.AgentId, r.OperationId).Error
		if err == nil {
			if existing.Revision != r.Revision || existing.ApprovalGeneration != r.ApprovalGeneration {
				return errors.New("operation ID reused for a different approval")
			}
			result, err = operationMessage(existing)
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := time.Now().UTC()
		if row.State != StateNeedsManualAuthorization || row.NonceUsedAt != nil || row.NonceExpires == nil || !now.Before(*row.NonceExpires) {
			return errors.New("approval is not executable, expired or already claimed")
		}
		a, err := Authorization(toRevision(row))
		if err != nil || a == nil {
			return errors.New("missing signed authorization")
		}
		required := make([]int32, 0, len(a.Actions))
		for _, action := range a.Actions {
			required = append(required, int32(action))
		}
		if !supportsActions(state, required) {
			return errors.New("required host action support has been withdrawn")
		}
		if a.ServiceId != state.ServiceID {
			return errors.New("service binding mismatch")
		}
		lease := now.Add(10 * time.Minute)
		if lease.After(*row.NonceExpires) {
			lease = *row.NonceExpires
		}
		op := models.ClientHostOperation{Client: r.AgentId, OperationID: r.OperationId, Revision: r.Revision, ApprovalGeneration: row.ApprovalGeneration, Authorization: row.Authorization, Phase: int32(hostv1.HostOperationPhase_HOST_OPERATION_PHASE_PREPARED), LeaseExpiresAt: lease, PriorStartupID: state.StartupID, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&op).Error; err != nil {
			return err
		}
		row.NonceUsedAt = &now
		row.State = StateApplying
		if err := advance(tx, &state, &row); err != nil {
			return err
		}
		result, err = operationMessage(op)
		return err
	})
	return result, err
}

func ReportOperation(db *gorm.DB, r *hostv1.ReportHostOperationRequest) (*hostv1.HostOperation, error) {
	var result *hostv1.HostOperation
	if len(r.Error) > 4096 || len(r.Operator) > 128 || len(r.LocalAuthentication) > 64 {
		return nil, errors.New("host report exceeds size limit")
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, r.AgentId)
		if err != nil {
			return err
		}
		var op models.ClientHostOperation
		if err := tx.First(&op, "client = ? AND operation_id = ?", r.AgentId, r.OperationId).Error; err != nil {
			return err
		}
		duplicate, err := receipt(tx, r.AgentId, r.ReceiptId, r)
		if err != nil {
			return err
		}
		if duplicate {
			result, err = operationMessage(op)
			return err
		}
		row, err := lockRevision(tx, r.AgentId, op.Revision)
		if err != nil {
			return err
		}
		if err := validateCurrent(state, row); err != nil {
			return err
		}
		if row.ApprovalGeneration != op.ApprovalGeneration {
			return errors.New("operation approval has been replaced")
		}
		phase := int32(r.Phase)
		if op.Phase >= 4 || (op.Phase == 3 && phase < 4) || phase < 2 || phase > 5 || phase < op.Phase || row.State == StateDelivered || (row.State == StateFailed && phase <= 3) {
			return errors.New("illegal host operation transition")
		}
		// Failure/restoration can be reported after expiry; success must happen in the lease.
		if phase <= 3 && !time.Now().Before(op.LeaseExpiresAt) {
			return errors.New("host execution lease has expired")
		}
		if phase == 3 && (strings.TrimSpace(r.Operator) == "" || strings.TrimSpace(r.LocalAuthentication) == "") {
			return errors.New("local OS authentication audit metadata is required")
		}
		op.Phase = phase
		op.Error = r.Error
		op.LocalAuthentication = r.LocalAuthentication
		op.Operator = r.Operator
		op.UpdatedAt = time.Now().UTC()
		if err := tx.Save(&op).Error; err != nil {
			return err
		}
		if phase >= 4 {
			row.State = StateFailed
			row.ErrorDetail = truncate(r.Error, 512)
			now := time.Now().UTC()
			row.FinishedAt = &now
		}
		if err := advance(tx, &state, &row); err != nil {
			return err
		}
		result, err = operationMessage(op)
		return err
	})
	return result, err
}

// Observe is the only v2 completion path. An authenticated report is checked
// against approval and host transaction; it is not a kernel attestation scheme.
func Observe(db *gorm.DB, id, key string, o *hostv1.DaemonObservation) (Revision, error) {
	var result Revision
	if o == nil || len(o.StartupId) > 128 || strings.TrimSpace(o.StartupId) == "" {
		return result, errors.New("daemon startup identity is required")
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, id)
		if err != nil {
			return err
		}
		duplicate, err := receipt(tx, id, key, o)
		if err != nil {
			return err
		}
		row, err := lockRevision(tx, id, o.Revision)
		if err != nil {
			return err
		}
		if duplicate {
			result = toRevision(row)
			return nil
		}
		if err := validateCurrent(state, row); err != nil {
			return err
		}
		if row.State != StateApplying && row.State != StateAwaitingAgent && row.State != StateDelivered {
			return errors.New("revision is not awaiting a daemon apply acknowledgement")
		}
		if o.ConfigHash != row.ConfigHash || o.ApprovalGeneration != row.ApprovalGeneration || o.ServiceId != state.ServiceID {
			return errors.New("observation does not match approval")
		}
		if o.ObservedAt == nil || o.ObservedAt.CheckValid() != nil || o.ObservedAt.AsTime().Before(*row.ConfirmedAt) || o.ObservedAt.AsTime().After(time.Now().Add(5*time.Minute)) {
			return errors.New("invalid observation time")
		}
		if int32(o.PrivilegeMode) != row.ToPrivilegeMode {
			return errors.New("daemon identity does not match target")
		}
		a, err := Authorization(toRevision(row))
		if err != nil || a == nil {
			return errors.New("missing authorization")
		}
		if len(a.Actions) > 0 {
			var op models.ClientHostOperation
			if err := tx.First(&op, "client = ? AND operation_id = ?", id, o.OperationId).Error; err != nil {
				return errors.New("matching host operation is required")
			}
			if op.Revision != row.Revision || op.ApprovalGeneration != row.ApprovalGeneration || op.Phase != 3 {
				return errors.New("host operation has not completed locally")
			}
			for _, action := range a.Actions {
				if action == hostv1.HostAction_HOST_ACTION_SET_SERVICE_IDENTITY && o.StartupId == op.PriorStartupID {
					return errors.New("identity conversion requires a restarted daemon")
				}
			}
		} else if row.NonceExpires == nil || !time.Now().Before(*row.NonceExpires) {
			return errors.New("approval has expired")
		}
		settings := decodeSettings(row.Config)
		c := o.EffectiveCapabilities
		if c == nil || c.PrivilegeMode != o.PrivilegeMode {
			return errors.New("effective capabilities are required")
		}
		available := func(c *reportv1.CapabilityState) bool { return c != nil && c.Available }
		if available(c.RemoteControl) != settings.RemoteControlEnabled || available(c.Webssh) != (settings.RemoteControlEnabled && settings.WebSSHEnabled) || available(c.Execution) != (settings.RemoteControlEnabled && settings.ExecutionEnabled) {
			return errors.New("effective remote capabilities do not match approved settings")
		}
		if !settings.EnableGPU && (available(c.Gpu) || available(c.DetailedGpu)) {
			return errors.New("GPU collection exceeds approval")
		}
		if available(c.RescueHelper) != settings.RescueHelperEnabled {
			return errors.New("effective helper capability does not match target")
		}
		if o.HelperAuthorized != settings.RescueHelperEnabled || o.HelperInstalled != settings.RescueHelperEnabled || (settings.RescueHelperEnabled && !o.HelperRunning) {
			return errors.New("helper installation/authorization does not match target")
		}
		if state.Observation != "" {
			var previous hostv1.DaemonObservation
			if e := protojson.Unmarshal([]byte(state.Observation), &previous); e != nil {
				return e
			}
			if previous.ObservedAt != nil && o.ObservedAt.AsTime().Before(previous.ObservedAt.AsTime()) {
				return errors.New("stale daemon observation")
			}
		}
		encoded, err := protojson.Marshal(o)
		if err != nil {
			return err
		}
		if err := clients.UpdateAppliedPrivilegedProfileWithDB(tx, id, settings.RemoteControlEnabled, settings.EnableGPU, settings.RescueHelperEnabled, int32(o.PrivilegeMode)); err != nil {
			return err
		}
		state.AppliedRevision = row.Revision
		state.Observation = string(encoded)
		state.PrivilegeMode = int32(o.PrivilegeMode)
		state.StartupID = o.StartupId
		row.State = StateDelivered
		row.ActivePrivilegeMode = int32(o.PrivilegeMode)
		row.ErrorDetail = ""
		now := time.Now().UTC()
		row.FinishedAt = &now
		if err := advance(tx, &state, &row); err != nil {
			return err
		}
		result = toRevision(row)
		return nil
	})
	return result, err
}

// FailApply records a bound application failure without inventing an observation
// or rolling the applied pointer back. Old and unapproved reports cannot use it.
func FailApply(db *gorm.DB, id string, revision uint64, key, hash string, generation uint64, detail string) (Revision, error) {
	var result Revision
	payload := &configv1.ReportPrivilegedDeliveryRequest{AgentId: id, Revision: revision, State: configv1.PrivilegedDeliveryState_PRIVILEGED_DELIVERY_STATE_FAILED, ProtocolVersion: 2, ConfigHash: hash, ApprovalGeneration: generation, ReceiptId: key, Errors: []*commonv1.ErrorDetail{{Message: detail}}}
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, id)
		if err != nil {
			return err
		}
		duplicate, err := receipt(tx, id, key, payload)
		if err != nil {
			return err
		}
		row, err := lockRevision(tx, id, revision)
		if err != nil {
			return err
		}
		if duplicate {
			result = toRevision(row)
			return nil
		}
		if err := validateCurrent(state, row); err != nil {
			return err
		}
		if hash != row.ConfigHash || generation != row.ApprovalGeneration {
			return errors.New("failure does not match approval")
		}
		if row.State != StateAwaitingAgent && row.State != StateApplying && row.State != StateNeedsManualAuthorization {
			return errors.New("terminal revision cannot be changed by a failure report")
		}
		if strings.TrimSpace(detail) == "" {
			return errors.New("application failure needs an error detail")
		}
		row.State = StateFailed
		row.ErrorDetail = truncate(detail, 512)
		now := time.Now().UTC()
		row.FinishedAt = &now
		if err := advance(tx, &state, &row); err != nil {
			return err
		}
		result = toRevision(row)
		return nil
	})
	return result, err
}
