// Package privilegeddelivery stores privileged configuration revisions and
// the human decisions attached to them.
//
// Settings/hash and target identity are immutable per revision. The exact host
// action list is frozen when panel approval is issued; receipt reports cannot
// reclassify that approval or change its authority.
package privilegeddelivery

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Upgrade classes and delivery states mirror the proto enums. They are
// retained as constants for database predicates and compatibility with stored rows.
const (
	ClassUnspecified      int32 = 0
	ClassAutomatic        int32 = 1
	ClassManualConfirm    int32 = 2
	ClassManualPrivileged int32 = 3

	StateUnspecified              int32 = 0
	StateNeedsConfirmation        int32 = 1
	StateNeedsManualAuthorization int32 = 2
	StateDelivered                int32 = 3
	StateRolledBack               int32 = 4
	StateFailed                   int32 = 5
	StateAwaitingAgent            int32 = 6
	StateApplying                 int32 = 7
	StateSuperseded               int32 = 8
	StateExpired                  int32 = 9

	privilegeUnspecified  int32 = 0
	privilegeLinuxRoot    int32 = 1
	privilegeLinuxNonRoot int32 = 2
	privilegeWindowsAdmin int32 = 3
	privilegeWindowsUser  int32 = 4

	// nonceLifetime bounds how long a manual upgrade task stays redeemable. A
	// task that sat unused for days is more likely forgotten than pending, and
	// a short window limits how long a leaked nonce is worth anything.
	nonceLifetime = 30 * time.Minute
)

// Settings is the privileged configuration carried by one revision.
type Settings struct {
	RemoteControlEnabled  bool  `json:"remote_control_enabled"`
	WebSSHEnabled         bool  `json:"webssh_enabled"`
	ExecutionEnabled      bool  `json:"execution_enabled"`
	EnableGPU             bool  `json:"enable_gpu"`
	RescueHelperEnabled   bool  `json:"rescue_helper_enabled"`
	RequiredPrivilegeMode int32 `json:"required_privilege_mode"`
}

// Revision is one stored revision with its decision state.
type Revision struct {
	Authorization       []byte
	ProtocolVersion     uint32
	ConfigHash          string
	ApprovalGeneration  uint64
	ConfirmedBy         string
	ReceivedAt          *time.Time
	UpdateSequence      uint64
	Client              string
	Revision            uint64
	Settings            Settings
	UpgradeClass        int32
	Reasons             []string
	FromPrivilegeMode   int32
	ToPrivilegeMode     int32
	State               int32
	TaskID              string
	Nonce               string
	NonceExpires        *time.Time
	NonceUsedAt         *time.Time
	Operator            string
	LocalAuthentication string
	ActivePrivilegeMode int32
	ErrorDetail         string
	PreviousRevision    uint64
	SavedAt             time.Time
	ConfirmedAt         *time.Time
	FinishedAt          *time.Time
	// UpgradeCommand is rendered rather than stored, so changing the install
	// URL does not require rewriting historical rows.
	UpgradeCommand string
}

// Save stores a new revision and classifies how it may be adopted.
//
// expectedRevision implements optimistic concurrency: two administrators
// editing the same machine must not silently overwrite each other, because the
// losing edit could be the one that narrowed privileges.
func Save(clientUUID string, settings Settings, expectedRevision uint64, reason string) (Revision, error) {
	return SaveWithDB(dbcore.GetDBInstance(), clientUUID, settings, expectedRevision, reason)
}

// SaveWithDB stores a new revision using the provided database connection.
func SaveWithDB(db *gorm.DB, clientUUID string, settings Settings, expectedRevision uint64, reason string) (Revision, error) {
	var result Revision
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, clientUUID)
		if err != nil {
			return err
		}
		if expectedRevision != state.DesiredRevision {
			return fmt.Errorf("privileged configuration conflict: expected %d, current %d", expectedRevision, state.DesiredRevision)
		}
		if (settings.WebSSHEnabled || settings.ExecutionEnabled) && !settings.RemoteControlEnabled {
			return errors.New("webssh and execution require remote control")
		}
		if settings.RescueHelperEnabled && settings.RemoteControlEnabled {
			return errors.New("rescue helper and ordinary remote control are mutually exclusive")
		}
		if settings.RequiredPrivilegeMode < 0 || settings.RequiredPrivilegeMode > 5 {
			return errors.New("unsupported privilege mode")
		}
		class, reasons, from, to := classify(tx, clientUUID, settings)
		now := time.Now().UTC()
		// A new desired revision invalidates old pending approvals atomically.
		if err := tx.Model(&models.ClientPrivilegedRevision{}).Where("client = ? AND state IN ?", clientUUID, []int32{StateNeedsConfirmation, StateNeedsManualAuthorization, StateAwaitingAgent, StateApplying, StateExpired}).Updates(map[string]any{"state": StateSuperseded, "superseded_at": now, "nonce": "", "update_sequence": state.UpdateSequence + 1}).Error; err != nil {
			return err
		}
		row := models.ClientPrivilegedRevision{Client: clientUUID, Revision: state.DesiredRevision + 1, Config: encodeSettings(settings), ConfigHash: settingsHash(settings), ProtocolVersion: 2, UpgradeClass: class, Reasons: encodeReasons(appendReason(reasons, reason)), FromPrivilegeMode: from, ToPrivilegeMode: to, State: StateNeedsConfirmation, PreviousRevision: state.AppliedRevision, SavedAt: now, CreatedAt: now, UpdateSequence: state.UpdateSequence + 1}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		state.DesiredRevision = row.Revision
		state.UpdateSequence++
		state.UpdatedAt = now
		if err := tx.Save(&state).Error; err != nil {
			return err
		}
		result = toRevision(row)
		return nil
	})
	return result, err
}

// Latest returns the newest revision above afterRevision.
func Latest(clientUUID string, afterRevision uint64) (Revision, bool, error) {
	var row models.ClientPrivilegedRevision
	err := dbcore.GetDBInstance().
		Where("client = ? AND revision > ?", clientUUID, afterRevision).
		Order("revision DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Revision{}, false, nil
	}
	if err != nil {
		return Revision{}, false, err
	}
	return toRevision(row), true, nil
}

// Current returns the newest revision regardless of what an Agent has seen.
func Current(clientUUID string) (Revision, bool, error) {
	return Latest(clientUUID, 0)
}

// History returns revisions newest first, for a panel timeline.
func History(clientUUID string, limit int) ([]Revision, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var rows []models.ClientPrivilegedRevision
	if err := dbcore.GetDBInstance().
		Where("client = ?", clientUUID).
		Order("revision DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	revisions := make([]Revision, 0, len(rows))
	for _, row := range rows {
		revisions = append(revisions, toRevision(row))
	}
	return revisions, nil
}

// Confirm records the panel-side confirmation.
//
// For a change within one privilege level this is the last step before the host
// applies it. For one that crosses a boundary it only unlocks the host-side
// upgrade: the state moves to awaiting manual authorization, and nothing here
// can move it further.
func Confirm(clientUUID string, revision uint64, userUUID string) (Revision, error) {
	return ConfirmWithDB(dbcore.GetDBInstance(), clientUUID, revision, userUUID)
}

func ConfirmWithDB(db *gorm.DB, clientUUID string, revision uint64, userUUID string) (Revision, error) {
	var result Revision
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, clientUUID)
		if err != nil {
			return err
		}
		row, err := lockRevision(tx, clientUUID, revision)
		if err != nil {
			return err
		}
		if state.DesiredRevision != revision || row.SupersededAt != nil {
			return errors.New("revision has been superseded")
		}
		if row.ProtocolVersion != 2 {
			return errors.New("legacy task must be superseded by a v2 revision")
		}
		if strings.TrimSpace(userUUID) == "" {
			return errors.New("an approving user is required")
		}
		now := time.Now().UTC()
		if row.State == StateDelivered {
			result = toRevision(row)
			return nil
		}
		if row.ConfirmedAt != nil && row.NonceExpires != nil && now.Before(*row.NonceExpires) && (row.State == StateNeedsManualAuthorization || row.State == StateAwaitingAgent || row.State == StateApplying || row.State == StateDelivered) {
			result = toRevision(row)
			return nil
		}
		if row.State != StateNeedsConfirmation && row.State != StateExpired && row.State != StateNeedsManualAuthorization && row.State != StateAwaitingAgent {
			return errors.New("revision cannot be confirmed in its current state")
		}
		if state.ProtocolVersion != 2 || state.ServiceID == "" {
			return errors.New("agent must negotiate privileged_delivery_v2 before approval")
		}
		actions, err := requiredActions(tx, state, row)
		if err != nil {
			return err
		}
		if !supportsActions(state, actions) {
			return errors.New("host does not support a required administrative action")
		}
		row.State = StateAwaitingAgent
		if len(actions) > 0 {
			row.State = StateNeedsManualAuthorization
			row.UpgradeClass = ClassManualPrivileged
		} else {
			row.UpgradeClass = ClassManualConfirm
		}
		row.ConfirmedAt = &now
		row.ConfirmedBy = userUUID
		row.ApprovalGeneration++
		row.TaskID = utils.GenerateRandomString(32)
		row.Nonce = utils.GenerateRandomString(43)
		row.NonceUsedAt = nil
		expiry := now.Add(nonceLifetime)
		row.NonceExpires = &expiry
		authorization := &hostv1.HostAuthorization{AgentId: clientUUID, Revision: revision, ConfigHash: row.ConfigHash, ApprovalGeneration: row.ApprovalGeneration, ServiceId: state.ServiceID, TargetPrivilegeMode: reportv1.PrivilegeMode(row.ToPrivilegeMode), Nonce: row.Nonce, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expiry), ApprovedBy: userUUID}
		for _, action := range actions {
			authorization.Actions = append(authorization.Actions, hostv1.HostAction(action))
		}
		row.Authorization, err = proto.MarshalOptions{Deterministic: true}.Marshal(authorization)
		if err != nil {
			return err
		}
		if err := advance(tx, &state, &row); err != nil {
			return err
		}
		result = toRevision(row)
		return nil
	})
	return result, err
}

// Report is a legacy transport receipt only. It cannot approve or complete a task.
func Report(clientUUID string, revision uint64, state, activeMode int32, detail string) (Revision, error) {
	return ReportWithDB(dbcore.GetDBInstance(), clientUUID, revision, state, activeMode, detail)
}

func ReportWithDB(db *gorm.DB, clientUUID string, revision uint64, event, activeMode int32, detail string) (Revision, error) {
	var result Revision
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, clientUUID)
		if err != nil {
			return err
		}
		row, err := lockRevision(tx, clientUUID, revision)
		if err != nil {
			return err
		}
		if event != StateNeedsConfirmation && event != StateNeedsManualAuthorization {
			return errors.New("legacy terminal reports are disabled; upgrade to privileged_delivery_v2")
		}
		if row.State == StateSuperseded {
			return errors.New("revision has been superseded")
		}
		if row.ReceivedAt == nil {
			now := time.Now().UTC()
			row.ReceivedAt = &now
			if err := advance(tx, &state, &row); err != nil {
				return err
			}
		}
		result = toRevision(row)
		return nil
	})
	return result, err
}

// RedeemNonce cannot establish OS authentication or daemon application. Legacy
// callers must upgrade; v2 records host progress and daemon observation separately.
func RedeemNonce(clientUUID, nonce, operator, localAuth string, resultingMode int32) (Revision, error) {
	return Revision{}, errors.New("legacy completion is disabled; use privileged_delivery_v2 host operations and daemon acknowledgement")
}

// classify predicts whether the target requires a host operation. Confirmation
// freezes the exact actions against the observed service and installed helper.
func classify(db *gorm.DB, clientUUID string, settings Settings) (class int32, reasons []string, from, to int32) {
	from = InstalledPrivilegeModeWithDB(db, clientUUID)
	to = settings.RequiredPrivilegeMode
	if to == privilegeUnspecified {
		to = from
	}
	if to != from || settings.RescueHelperEnabled {
		return ClassManualPrivileged, []string{"service identity changes and helper installation require local administrator authorization"}, from, to
	}
	return ClassManualConfirm, []string{"same-identity capability changes require signed panel approval"}, from, to
}

// InstalledPrivilegeMode uses the v2 service observation when available, then
// legacy installation metadata. Neither describes the invoking CLI's euid.
func InstalledPrivilegeMode(clientUUID string) int32 {
	return InstalledPrivilegeModeWithDB(dbcore.GetDBInstance(), clientUUID)
}

// InstalledPrivilegeModeWithDB reports what the Agent service is installed as using the provided DB/tx instance.
func InstalledPrivilegeModeWithDB(db *gorm.DB, clientUUID string) int32 {
	var state models.ClientPrivilegedState
	if err := db.First(&state, "client = ?", clientUUID).Error; err == nil && state.ProtocolVersion == 2 && state.PrivilegeMode != 0 {
		return state.PrivilegeMode
	}
	profile, saved, err := clients.GetDeploymentProfileWithDB(db, clientUUID)
	if err != nil || !saved {
		return privilegeUnspecified
	}
	windows := strings.EqualFold(profile.Platform, "windows")
	if profile.RuntimeIdentity == clients.AgentRuntimeIdentityServiceAccount {
		if windows {
			return privilegeWindowsUser
		}
		return privilegeLinuxNonRoot
	}
	if windows {
		return privilegeWindowsAdmin
	}
	return privilegeLinuxRoot
}

func lockRevision(tx *gorm.DB, clientUUID string, revision uint64) (models.ClientPrivilegedRevision, error) {
	var row models.ClientPrivilegedRevision
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("client = ? AND revision = ?", clientUUID, revision).First(&row).Error
	if err != nil {
		return row, fmt.Errorf("privileged revision %d was not found for this machine", revision)
	}
	return row, nil
}

func toRevision(row models.ClientPrivilegedRevision) Revision {
	revision := Revision{
		Authorization: row.Authorization, ProtocolVersion: row.ProtocolVersion, ConfigHash: row.ConfigHash, ApprovalGeneration: row.ApprovalGeneration, ConfirmedBy: row.ConfirmedBy, ReceivedAt: row.ReceivedAt, UpdateSequence: row.UpdateSequence,
		Client:              row.Client,
		Revision:            row.Revision,
		Settings:            decodeSettings(row.Config),
		UpgradeClass:        row.UpgradeClass,
		Reasons:             decodeReasons(row.Reasons),
		FromPrivilegeMode:   row.FromPrivilegeMode,
		ToPrivilegeMode:     row.ToPrivilegeMode,
		State:               row.State,
		TaskID:              row.TaskID,
		Nonce:               row.Nonce,
		NonceExpires:        row.NonceExpires,
		NonceUsedAt:         row.NonceUsedAt,
		Operator:            row.Operator,
		LocalAuthentication: row.LocalAuthentication,
		ActivePrivilegeMode: row.ActivePrivilegeMode,
		ErrorDetail:         row.ErrorDetail,
		PreviousRevision:    row.PreviousRevision,
		SavedAt:             row.SavedAt,
		ConfirmedAt:         row.ConfirmedAt,
		FinishedAt:          row.FinishedAt,
	}
	if row.UpgradeClass == ClassManualPrivileged {
		revision.UpgradeCommand = fmt.Sprintf("sudo komari-host apply --revision %d", row.Revision)
	}
	return revision
}

func encodeSettings(settings Settings) string {
	data, err := json.Marshal(settings)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func decodeSettings(value string) Settings {
	var settings Settings
	if strings.TrimSpace(value) == "" {
		return settings
	}
	_ = json.Unmarshal([]byte(value), &settings)
	return settings
}

func encodeReasons(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	data, err := json.Marshal(reasons)
	if err != nil {
		return ""
	}
	return string(data)
}

func decodeReasons(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var reasons []string
	if err := json.Unmarshal([]byte(value), &reasons); err != nil {
		return nil
	}
	return reasons
}

func appendReason(reasons []string, reason string) []string {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		return reasons
	}
	return append(reasons, truncate(trimmed, 256))
}

func truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit]
}
