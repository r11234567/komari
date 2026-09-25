// Package privilegeddelivery stores privileged configuration revisions and
// the human decisions attached to them.
//
// The classification is computed once, when a revision is saved, and then
// frozen. That matters: the class decides whether a panel confirmation is
// enough or an operator must authenticate on the host, and recomputing it
// later would let a privilege boundary be crossed because an Agent's reported
// mode happened to change in between.
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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Upgrade classes and delivery states mirror the proto enums. They are
// duplicated as constants so the database layer does not depend on generated
// protobuf code.
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

	privilegeUnspecified  int32 = 0
	privilegeLinuxRoot    int32 = 1
	privilegeLinuxNonRoot int32 = 2
	privilegeWindowsAdmin int32 = 3
	privilegeWindowsUser  int32 = 4

	// nonceLifetime bounds how long a manual upgrade task stays redeemable. A
	// task that sat unused for days is more likely forgotten than pending, and
	// a short window limits how long a leaked nonce is worth anything.
	nonceLifetime = 24 * time.Hour
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
	var result Revision
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		latest, found, err := latestLocked(tx, clientUUID)
		if err != nil {
			return err
		}
		current := uint64(0)
		if found {
			current = latest.Revision
		}
		if expectedRevision != 0 && expectedRevision != current {
			return fmt.Errorf("privileged configuration changed since it was read (expected revision %d, current %d)",
				expectedRevision, current)
		}

		class, reasons, from, to := classify(clientUUID, settings)
		now := time.Now().UTC()
		row := models.ClientPrivilegedRevision{
			Client:            clientUUID,
			Revision:          current + 1,
			Config:            encodeSettings(settings),
			UpgradeClass:      class,
			Reasons:           encodeReasons(appendReason(reasons, reason)),
			FromPrivilegeMode: from,
			ToPrivilegeMode:   to,
			State:             StateNeedsConfirmation,
			PreviousRevision:  current,
			SavedAt:           now,
			CreatedAt:         now,
		}
		// A manual-privileged revision gets its nonce at save time rather than
		// at confirmation, so the panel can show the operator the exact command
		// before anyone commits to the change.
		if class == ClassManualPrivileged {
			expiry := now.Add(nonceLifetime)
			row.TaskID = utils.GenerateRandomString(32)
			row.Nonce = utils.GenerateRandomString(43)
			row.NonceExpires = &expiry
		}
		if err := tx.Create(&row).Error; err != nil {
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
	var result Revision
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		row, err := lockRevision(tx, clientUUID, revision)
		if err != nil {
			return err
		}
		if row.State != StateNeedsConfirmation {
			return fmt.Errorf("revision %d is not awaiting confirmation", revision)
		}
		now := time.Now().UTC()
		// Confirmation never completes a privileged change, whatever its class.
		// Even within one privilege level the host still has to apply it and
		// say so, because the panel cannot observe that the Agent restarted
		// and adopted the settings; reporting is what closes the loop.
		next := StateNeedsManualAuthorization
		updates := map[string]any{
			"state": next, "confirmed_at": now, "updated_at": now,
		}
		if err := tx.Model(&row).Updates(updates).Error; err != nil {
			return err
		}
		row.State = next
		row.ConfirmedAt = &now
		result = toRevision(row)
		return nil
	})
	return result, err
}

// Report records an outcome reported by the host.
func Report(clientUUID string, revision uint64, state, activeMode int32, detail string) (Revision, error) {
	var result Revision
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		row, err := lockRevision(tx, clientUUID, revision)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		updates := map[string]any{
			"state":                 state,
			"active_privilege_mode": activeMode,
			"error_detail":          truncate(detail, 512),
			"updated_at":            now,
		}
		if state == StateDelivered || state == StateRolledBack || state == StateFailed {
			updates["finished_at"] = now
		}
		if err := tx.Model(&row).Updates(updates).Error; err != nil {
			return err
		}
		row.State = state
		row.ActivePrivilegeMode = activeMode
		row.ErrorDetail = truncate(detail, 512)
		if state == StateDelivered || state == StateRolledBack || state == StateFailed {
			row.FinishedAt = &now
		}
		result = toRevision(row)
		return nil
	})
	return result, err
}

// RedeemNonce marks a manual upgrade as completed on the host.
//
// The nonce is single-use, which is what makes this evidence rather than a
// claim: it was delivered to one machine, and redeeming it proves the upgrade
// ran there rather than being asserted from somewhere else.
func RedeemNonce(clientUUID, nonce, operator, localAuth string, resultingMode int32) (Revision, error) {
	var result Revision
	trimmed := strings.TrimSpace(nonce)
	if trimmed == "" {
		return result, errors.New("an upgrade task nonce is required")
	}
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		var row models.ClientPrivilegedRevision
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("client = ? AND nonce = ?", clientUUID, trimmed).First(&row).Error; err != nil {
			return errors.New("this upgrade task is not recognized for this machine")
		}
		if row.NonceUsedAt != nil {
			return errors.New("this upgrade task was already completed")
		}
		now := time.Now().UTC()
		if row.NonceExpires != nil && now.After(*row.NonceExpires) {
			return errors.New("this upgrade task has expired; deliver the change again")
		}
		updates := map[string]any{
			"nonce_used_at":         now,
			"operator":              truncate(operator, 128),
			"local_authentication":  truncate(localAuth, 64),
			"active_privilege_mode": resultingMode,
			"state":                 StateDelivered,
			"finished_at":           now,
			"updated_at":            now,
		}
		if err := tx.Model(&row).Updates(updates).Error; err != nil {
			return err
		}
		row.NonceUsedAt = &now
		row.Operator = truncate(operator, 128)
		row.LocalAuthentication = truncate(localAuth, 64)
		row.ActivePrivilegeMode = resultingMode
		row.State = StateDelivered
		row.FinishedAt = &now
		result = toRevision(row)
		return nil
	})
	return result, err
}

// classify decides how a revision may be adopted.
//
// Anything that turns on a capability executing commands with the Agent's
// identity is privileged. Whether it also needs an on-host upgrade depends on
// what the Agent is installed as: a service account cannot gain those
// capabilities without being reinstalled, so the panel cannot deliver them by
// itself no matter how they are confirmed.
func classify(clientUUID string, settings Settings) (class int32, reasons []string, from, to int32) {
	from = InstalledPrivilegeMode(clientUUID)
	to = from

	widening := settings.RemoteControlEnabled || settings.WebSSHEnabled ||
		settings.ExecutionEnabled || settings.RescueHelperEnabled

	if !widening {
		// Narrowing or GPU-only changes still take a confirmation. They are
		// delivered on the privileged track because they belong to the same
		// settings group, and a confirmation is cheap compared to discovering
		// that remote control was switched off by accident.
		return ClassManualConfirm, []string{"privileged settings always require an explicit confirmation"}, from, to
	}

	required := settings.RequiredPrivilegeMode
	if required == privilegeUnspecified {
		required = privilegedEquivalent(from)
	}
	to = required

	if isPrivilegedMode(from) {
		reasons = append(reasons, "the Agent already runs privileged, so this needs a confirmation in the panel")
		return ClassManualConfirm, reasons, from, to
	}

	reasons = append(reasons,
		"remote control, WebSSH, execution and the rescue helper run commands with the Agent's own identity",
		"this Agent runs as an unprivileged service account, so it must be upgraded on the host",
		"the operator authenticates on the machine; no credential is sent to the panel")
	return ClassManualPrivileged, reasons, from, to
}

// installedPrivilegeMode reads what the Agent was installed as.
//
// The deployment profile is the authoritative answer rather than the Agent's
// last report: a report describes a process that may already have been
// restarted, while the profile describes what the service will be next time it
// starts, which is what an upgrade has to change.
// InstalledPrivilegeMode reports what the Agent service is installed as.
func InstalledPrivilegeMode(clientUUID string) int32 {
	profile, saved, err := clients.GetDeploymentProfile(clientUUID)
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

func privilegedEquivalent(mode int32) int32 {
	switch mode {
	case privilegeWindowsUser, privilegeWindowsAdmin:
		return privilegeWindowsAdmin
	default:
		return privilegeLinuxRoot
	}
}

func isPrivilegedMode(mode int32) bool {
	return mode == privilegeLinuxRoot || mode == privilegeWindowsAdmin
}

func latestLocked(tx *gorm.DB, clientUUID string) (models.ClientPrivilegedRevision, bool, error) {
	var row models.ClientPrivilegedRevision
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("client = ?", clientUUID).Order("revision DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, true, nil
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
		revision.UpgradeCommand = "sudo komari-agent upgrade"
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
