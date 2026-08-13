package rescueapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils"
	deploymentapp "github.com/komari-monitor/komari/web/deployment"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	rescuev1 "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultTimeout   = 2 * time.Minute
	maxTimeout       = 10 * time.Minute
	defaultOutput    = 64 << 10
	maximumOutput    = 1 << 20
	maxArgumentCount = 16
	maxArgumentBytes = 4096
)

var signals = struct {
	sync.Mutex
	byKey map[string]chan struct{}
}{byKey: make(map[string]chan struct{})}

var createMu sync.Mutex

func signalKey(key string) {
	signals.Lock()
	if current := signals.byKey[key]; current != nil {
		close(current)
	}
	signals.byKey[key] = make(chan struct{})
	signals.Unlock()
}

func signalFor(key string) chan struct{} {
	signals.Lock()
	defer signals.Unlock()
	current := signals.byKey[key]
	if current == nil {
		current = make(chan struct{})
		signals.byKey[key] = current
	}
	return current
}

func GetStatus(agentID string) (*rescuev1.RescueHelperStatus, error) {
	profile, _, err := clients.GetDeploymentProfile(agentID)
	if err != nil {
		return nil, err
	}
	status := &rescuev1.RescueHelperStatus{Requested: profile.RescueEnabled}
	var stored models.ClientRescueHelper
	if err := dbcore.GetDBInstance().First(&stored, "client = ?", agentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return status, nil
		}
		return nil, err
	}
	status.Installed = stored.Installed
	status.GuardianRunning = stored.GuardianRunning
	status.HelperRunning = stored.HelperRunning
	status.NetworkIsolation = rescuev1.NetworkIsolationMode(stored.NetworkIsolation)
	if stored.BlockedInterfaces != "" {
		_ = json.Unmarshal([]byte(stored.BlockedInterfaces), &status.BlockedInterfaces)
	}
	status.Version = stored.Version
	status.HelperInstanceId = stored.HelperInstanceID
	status.ObservedAt = timestamppb.New(stored.ObservedAt)
	if stored.ErrorCode != "" || stored.ErrorMessage != "" {
		status.Error = &commonv1.ErrorDetail{Code: stored.ErrorCode, Message: stored.ErrorMessage}
	}
	return status, nil
}

func ReportStatus(agentID string, status *rescuev1.RescueHelperStatus) error {
	if status == nil {
		return errors.New("rescue helper status is required")
	}
	if _, _, err := clients.GetDeploymentProfile(agentID); err != nil {
		return err
	}
	status.HelperInstanceId = sanitize(status.HelperInstanceId, 128)
	if (status.Installed || status.GuardianRunning || status.HelperRunning) && status.HelperInstanceId == "" {
		return errors.New("helper instance ID is required for an installed rescue helper")
	}
	observedAt := time.Now().UTC()
	if status.ObservedAt != nil && status.ObservedAt.IsValid() {
		observedAt = status.ObservedAt.AsTime().UTC()
	}
	errorCode, errorMessage := "", ""
	if status.Error != nil {
		errorCode = sanitize(status.Error.Code, 64)
		errorMessage = sanitize(status.Error.Message, 512)
	}
	blockedInterfaces, _ := json.Marshal(sanitizeList(status.BlockedInterfaces, 128, 256))
	row := models.ClientRescueHelper{
		Client: agentID, Installed: status.Installed, GuardianRunning: status.GuardianRunning,
		HelperRunning: status.HelperRunning, NetworkIsolation: int32(status.NetworkIsolation), BlockedInterfaces: string(blockedInterfaces),
		Version: sanitize(status.Version, 100), HelperInstanceID: status.HelperInstanceId,
		ErrorCode: errorCode, ErrorMessage: errorMessage, ObservedAt: observedAt,
	}
	return dbcore.GetDBInstance().Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "client"}},
		UpdateAll: true,
	}).Create(&row).Error
}

func ValidateHelper(agentID, helperInstanceID string) error {
	helperInstanceID = sanitize(helperInstanceID, 128)
	if helperInstanceID == "" {
		return errors.New("helper instance ID is required")
	}
	profile, saved, err := clients.GetDeploymentProfile(agentID)
	if err != nil {
		return err
	}
	if !saved || profile.RemoteControlEnabled || !profile.RescueEnabled {
		return errors.New("rescue helper is not enabled in the installed deployment profile")
	}
	var stored models.ClientRescueHelper
	if err := dbcore.GetDBInstance().First(&stored, "client = ?", agentID).Error; err != nil {
		return err
	}
	if !stored.Installed || !stored.GuardianRunning || !stored.HelperRunning {
		return errors.New("rescue helper is not running")
	}
	if stored.HelperInstanceID != helperInstanceID {
		return errors.New("rescue helper instance does not match the active helper")
	}
	return nil
}

// ClearConnectionError removes a stale transport error once the same helper
// successfully establishes a new lease stream.
func ClearConnectionError(agentID, helperInstanceID string) error {
	return dbcore.GetDBInstance().Model(&models.ClientRescueHelper{}).
		Where("client = ? AND helper_instance_id = ? AND error_code = ?", agentID, sanitize(helperInstanceID, 128), "HELPER_CONNECTION").
		Updates(map[string]any{
			"error_code": "", "error_message": "", "observed_at": time.Now().UTC(),
		}).Error
}

func Create(agentID string, action rescuev1.RescueAction, arguments []string, timeout *durationpb.Duration, maxOutput uint64, idempotencyKey string) (*rescuev1.RescueSession, error) {
	createMu.Lock()
	defer createMu.Unlock()
	profile, saved, err := clients.GetDeploymentProfile(agentID)
	if err != nil {
		return nil, err
	}
	if !saved || profile.RemoteControlEnabled || !profile.RescueEnabled {
		return nil, errors.New("rescue helper is unavailable unless normal remote control is disabled and rescue is enabled")
	}
	if !allowedAction(action) {
		return nil, errors.New("unsupported rescue action")
	}
	if len(arguments) > maxArgumentCount {
		return nil, errors.New("too many rescue arguments")
	}
	total := 0
	for i := range arguments {
		arguments[i] = sanitize(arguments[i], maxArgumentBytes)
		total += len(arguments[i])
	}
	if total > maxArgumentBytes {
		return nil, errors.New("rescue arguments are too large")
	}
	duration := defaultTimeout
	if timeout != nil {
		if err := timeout.CheckValid(); err != nil {
			return nil, err
		}
		duration = timeout.AsDuration()
	}
	if duration <= 0 || duration > maxTimeout {
		return nil, fmt.Errorf("rescue timeout must be between 1s and %s", maxTimeout)
	}
	if maxOutput == 0 {
		maxOutput = defaultOutput
	}
	if maxOutput > maximumOutput {
		return nil, fmt.Errorf("rescue output exceeds %d bytes", maximumOutput)
	}
	idempotencyKey = sanitize(idempotencyKey, 128)
	if idempotencyKey != "" {
		var existing models.RescueSession
		if err := dbcore.GetDBInstance().Where("client = ? AND idempotency_key = ?", agentID, idempotencyKey).First(&existing).Error; err == nil {
			return sessionToProto(existing), nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	encodedArguments, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	row := models.RescueSession{
		ID: utils.GenerateRandomString(32), Client: agentID, Action: int32(action), Arguments: string(encodedArguments),
		State: int32(commonv1.OperationState_OPERATION_STATE_QUEUED), TimeoutSeconds: int64(duration / time.Second),
		MaxOutputBytes: maxOutput, IdempotencyKey: idempotencyKey, CreatedAt: now,
	}
	if row.ID == "" {
		return nil, errors.New("failed to create rescue session ID")
	}
	if err := dbcore.GetDBInstance().Create(&row).Error; err != nil {
		return nil, err
	}
	if action == rescuev1.RescueAction_RESCUE_ACTION_ROLLBACK_ONLINE_CONFIG {
		if _, err := deploymentapp.RollbackOnlineConfig(agentID); err != nil {
			_ = dbcore.GetDBInstance().Delete(&row).Error
			return nil, fmt.Errorf("rollback online configuration: %w", err)
		}
	}
	signalKey("lease:" + agentID)
	return sessionToProto(row), nil
}

func allowedAction(action rescuev1.RescueAction) bool {
	switch action {
	case rescuev1.RescueAction_RESCUE_ACTION_DIAGNOSTICS,
		rescuev1.RescueAction_RESCUE_ACTION_SHUTDOWN,
		rescuev1.RescueAction_RESCUE_ACTION_REBOOT,
		rescuev1.RescueAction_RESCUE_ACTION_BLOCK_PUBLIC_INTERFACES,
		rescuev1.RescueAction_RESCUE_ACTION_BLOCK_TAILSCALE_INTERFACES,
		rescuev1.RescueAction_RESCUE_ACTION_ISOLATE_CONTROL_PLANE,
		rescuev1.RescueAction_RESCUE_ACTION_RESTORE_NETWORK,
		rescuev1.RescueAction_RESCUE_ACTION_ROLLBACK_ONLINE_CONFIG:
		return true
	default:
		return false
	}
}

func sanitizeList(values []string, maxCount, maxBytes int) []string {
	if len(values) > maxCount {
		values = values[:maxCount]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = sanitize(value, maxBytes); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func GetSession(sessionID string) (*rescuev1.RescueSession, error) {
	var row models.RescueSession
	if err := dbcore.GetDBInstance().First(&row, "id = ?", sessionID).Error; err != nil {
		return nil, err
	}
	return sessionToProto(row), nil
}

func Cancel(sessionID, reason string) (*rescuev1.RescueSession, error) {
	reason = sanitize(reason, 512)
	agentID, helperInstanceID := "", ""
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		var session models.RescueSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", sessionID).Error; err != nil {
			return err
		}
		state := commonv1.OperationState(session.State)
		agentID = session.Client
		helperInstanceID = session.HelperInstanceID
		if terminal(state) || state == commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED {
			return nil
		}
		now := time.Now().UTC()
		updates := map[string]any{
			"error_message": reason,
			"updated_at":    now,
		}
		if state == commonv1.OperationState_OPERATION_STATE_QUEUED {
			updates["state"] = int32(commonv1.OperationState_OPERATION_STATE_CANCELLED)
			updates["error_code"] = "CANCELLED"
			updates["finished_at"] = now
		} else {
			updates["state"] = int32(commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED)
			updates["error_code"] = "CANCEL_REQUESTED"
		}
		if err := tx.Model(&models.RescueSession{}).Where("id = ? AND state = ?", sessionID, session.State).Updates(updates).Error; err != nil {
			return err
		}
		if state == commonv1.OperationState_OPERATION_STATE_QUEUED {
			return tx.Create(&models.RescueEvent{
				Session: sessionID, Sequence: 1, OccurredAt: now,
				State:     int32(commonv1.OperationState_OPERATION_STATE_CANCELLED),
				ErrorCode: "CANCELLED", ErrorMessage: reason,
			}).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	signalKey("session:" + sessionID)
	if helperInstanceID != "" {
		signalKey("lease:" + agentID)
	}
	return GetSession(sessionID)
}

func NextAssignment(agentID, helperInstanceID, afterAssignmentID string) (*rescuev1.RescueAssignment, error) {
	var row models.RescueSession
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		if afterAssignmentID != "" {
			var previous models.RescueSession
			if err := tx.First(&previous, "id = ? AND client = ? AND helper_instance_id = ?", afterAssignmentID, agentID, helperInstanceID).Error; err == nil {
				switch commonv1.OperationState(previous.State) {
				case commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED:
					row = previous
					return nil
				case commonv1.OperationState_OPERATION_STATE_RUNNING:
					return gorm.ErrRecordNotFound
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		if err := tx.Where("client = ? AND state = ?", agentID, int32(commonv1.OperationState_OPERATION_STATE_QUEUED)).
			Order("created_at ASC").First(&row).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		result := tx.Model(&models.RescueSession{}).Where("id = ? AND state = ?", row.ID, int32(commonv1.OperationState_OPERATION_STATE_QUEUED)).
			Updates(map[string]any{
				"state": int32(commonv1.OperationState_OPERATION_STATE_RUNNING), "helper_instance_id": helperInstanceID,
				"started_at": now, "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		row.State = int32(commonv1.OperationState_OPERATION_STATE_RUNNING)
		row.HelperInstanceID = helperInstanceID
		row.StartedAt = &now
		row.UpdatedAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	expires := time.Now().UTC().Add(time.Duration(row.TimeoutSeconds) * time.Second)
	return &rescuev1.RescueAssignment{AssignmentId: row.ID, Session: sessionToProto(row), LeaseExpiresAt: timestamppb.New(expires)}, nil
}

func EventsAfter(sessionID string, sequence uint64) ([]*rescuev1.RescueEvent, error) {
	var rows []models.RescueEvent
	if err := dbcore.GetDBInstance().Where("session = ? AND sequence > ?", sessionID, sequence).Order("sequence ASC").Limit(128).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]*rescuev1.RescueEvent, 0, len(rows))
	for _, row := range rows {
		result = append(result, eventToProto(row))
	}
	return result, nil
}

func ExpireIfOverdue(sessionID string) (*rescuev1.RescueSession, bool, error) {
	var expired bool
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		var session models.RescueSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", sessionID).Error; err != nil {
			return err
		}
		if session.StartedAt == nil || terminal(commonv1.OperationState(session.State)) {
			return nil
		}
		deadline := session.StartedAt.Add(time.Duration(session.TimeoutSeconds) * time.Second)
		if time.Now().UTC().Before(deadline) {
			return nil
		}
		var latest uint64
		if err := tx.Model(&models.RescueEvent{}).Where("session = ?", sessionID).Select("COALESCE(MAX(sequence), 0)").Scan(&latest).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Create(&models.RescueEvent{
			Session: sessionID, Sequence: latest + 1, OccurredAt: now,
			State:     int32(commonv1.OperationState_OPERATION_STATE_DEADLINE_EXCEEDED),
			ErrorCode: "DEADLINE_EXCEEDED", ErrorMessage: "rescue operation exceeded its deadline",
		}).Error; err != nil {
			return err
		}
		result := tx.Model(&models.RescueSession{}).Where("id = ? AND state = ?", sessionID, session.State).Updates(map[string]any{
			"state": int32(commonv1.OperationState_OPERATION_STATE_DEADLINE_EXCEEDED), "finished_at": now,
			"error_code": "DEADLINE_EXCEEDED", "error_message": "rescue operation exceeded its deadline", "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("rescue session state changed while expiring the operation")
		}
		expired = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if expired {
		signalKey("session:" + sessionID)
	}
	session, err := GetSession(sessionID)
	return session, expired, err
}

func ReportEvent(agentID, helperInstanceID string, event *rescuev1.RescueEvent) (uint64, error) {
	if event == nil || event.SessionId == "" || event.Sequence == 0 {
		return 0, errors.New("session ID and sequence are required")
	}
	when := time.Now().UTC()
	if event.OccurredAt != nil && event.OccurredAt.IsValid() {
		when = event.OccurredAt.AsTime().UTC()
	}
	errorCode, errorMessage := "", ""
	if event.Error != nil {
		errorCode = sanitize(event.Error.Code, 64)
		errorMessage = sanitize(event.Error.Message, 512)
	}
	row := models.RescueEvent{
		Session: event.SessionId, Sequence: event.Sequence, OccurredAt: when, State: int32(event.State),
		Stream: int32(event.Stream), Output: append([]byte(nil), event.Output...), ErrorCode: errorCode, ErrorMessage: errorMessage,
	}
	acceptedSequence := uint64(0)
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		var session models.RescueSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ? AND client = ?", event.SessionId, agentID).Error; err != nil {
			return err
		}
		if session.HelperInstanceID == "" || session.HelperInstanceID != helperInstanceID {
			return errors.New("rescue session is not assigned to this helper instance")
		}
		var latest uint64
		if err := tx.Model(&models.RescueEvent{}).Where("session = ?", event.SessionId).Select("COALESCE(MAX(sequence), 0)").Scan(&latest).Error; err != nil {
			return err
		}
		if event.Sequence <= latest {
			acceptedSequence = latest
			return nil
		}
		if event.Sequence != latest+1 {
			return errors.New("rescue event sequence has a gap")
		}
		if uint64(len(event.Output))+session.OutputBytes > session.MaxOutputBytes {
			return errors.New("rescue output limit exceeded")
		}
		if terminal(commonv1.OperationState(session.State)) {
			return errors.New("rescue session is already terminal")
		}
		currentState := commonv1.OperationState(session.State)
		if event.State != commonv1.OperationState_OPERATION_STATE_RUNNING && !terminal(event.State) {
			return errors.New("rescue event must report running or a terminal state")
		}
		if currentState == commonv1.OperationState_OPERATION_STATE_CANCEL_REQUESTED && event.State == commonv1.OperationState_OPERATION_STATE_RUNNING {
			return errors.New("a cancelled rescue session cannot return to running")
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		updates := map[string]any{"state": int32(event.State), "output_bytes": session.OutputBytes + uint64(len(event.Output)), "updated_at": when}
		if session.StartedAt == nil && event.State == commonv1.OperationState_OPERATION_STATE_RUNNING {
			updates["started_at"] = when
		}
		if terminal(event.State) {
			updates["finished_at"] = when
			updates["error_code"] = errorCode
			updates["error_message"] = errorMessage
		}
		result := tx.Model(&models.RescueSession{}).Where("id = ? AND state = ?", event.SessionId, session.State).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("rescue session state changed while accepting the event")
		}
		acceptedSequence = event.Sequence
		return nil
	})
	if err != nil {
		return 0, err
	}
	signalKey("session:" + event.SessionId)
	return acceptedSequence, nil
}

func WaitSignal(key string) chan struct{} { return signalFor(key) }

func sessionToProto(row models.RescueSession) *rescuev1.RescueSession {
	arguments := []string{}
	_ = json.Unmarshal([]byte(row.Arguments), &arguments)
	result := &rescuev1.RescueSession{
		SessionId: row.ID, AgentId: row.Client, Action: rescuev1.RescueAction(row.Action), Arguments: arguments,
		State: commonv1.OperationState(row.State), CreatedAt: timestamppb.New(row.CreatedAt), OutputBytes: row.OutputBytes,
	}
	if row.StartedAt != nil {
		result.StartedAt = timestamppb.New(*row.StartedAt)
		result.DeadlineAt = timestamppb.New(row.StartedAt.Add(time.Duration(row.TimeoutSeconds) * time.Second))
	}
	if row.FinishedAt != nil {
		result.FinishedAt = timestamppb.New(*row.FinishedAt)
	}
	if row.ErrorCode != "" || row.ErrorMessage != "" {
		result.Error = &commonv1.ErrorDetail{Code: row.ErrorCode, Message: row.ErrorMessage}
	}
	return result
}

func eventToProto(row models.RescueEvent) *rescuev1.RescueEvent {
	result := &rescuev1.RescueEvent{
		SessionId: row.Session, Sequence: row.Sequence, OccurredAt: timestamppb.New(row.OccurredAt),
		State: commonv1.OperationState(row.State), Stream: rescuev1.RescueOutputStream(row.Stream), Output: append([]byte(nil), row.Output...),
	}
	if row.ErrorCode != "" || row.ErrorMessage != "" {
		result.Error = &commonv1.ErrorDetail{Code: row.ErrorCode, Message: row.ErrorMessage}
	}
	return result
}

func terminal(state commonv1.OperationState) bool {
	return state == commonv1.OperationState_OPERATION_STATE_CANCELLED ||
		state == commonv1.OperationState_OPERATION_STATE_DEADLINE_EXCEEDED ||
		state == commonv1.OperationState_OPERATION_STATE_FAILED ||
		state == commonv1.OperationState_OPERATION_STATE_SUCCEEDED
}

func sanitize(value string, limit int) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value))
	for len(value) > limit {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
