package rescueapp

import (
	"errors"
	"net/netip"
	"regexp"
	"time"

	"github.com/komari-monitor/komari/database/models"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	rescuev1 "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const grantSSHAction = rescuev1.RescueAction_RESCUE_ACTION_TEMPORARY_SSH_ACCESS
const revokeSSHAction = rescuev1.RescueAction_RESCUE_ACTION_REVOKE_TEMPORARY_SSH_ACCESS
const sshWindowLifetime = 15 * time.Minute

var sshGrantID = regexp.MustCompile(`^[A-Za-z0-9]{32}$`)
var sshEnrollmentKeyID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func SSHWindowTarget(window *rescuev1.SSHWindowRequest) []string {
	if window == nil {
		return nil
	}
	return []string{window.GetSourceIp(), window.GetPreviousGrantId(), window.GetEnrollmentKeyId()}
}

func validateSSHWindowSpec(action rescuev1.RescueAction, window *rescuev1.SSHWindowRequest, port uint32) error {
	if action != grantSSHAction && action != revokeSSHAction {
		if window != nil {
			return errors.New("temporary SSH settings only apply to temporary SSH actions")
		}
		return nil
	}
	if window == nil || !sshEnrollmentKeyID.MatchString(window.GetEnrollmentKeyId()) {
		return errors.New("temporary SSH requires an updated panel and helper with enrollment binding")
	}
	if previous := window.GetPreviousGrantId(); previous != "" && !sshGrantID.MatchString(previous) {
		return errors.New("invalid temporary SSH authorization ID")
	}
	if action == revokeSSHAction {
		if window.GetPreviousGrantId() == "" || window.GetSourceIp() != "" {
			return errors.New("select the temporary SSH authorization to revoke")
		}
		return nil
	}
	source, err := netip.ParseAddr(window.GetSourceIp())
	if err != nil || !source.IsGlobalUnicast() || source.Is4In6() || source.Zone() != "" || port == 0 {
		return errors.New("temporary SSH requires the node's fixed port and a single source IPv4 or IPv6 address")
	}
	return nil
}

func authorizeSSHWindow(tx *gorm.DB, session models.RescueSession) error {
	var identity models.AgentCredential
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("agent_key_id", "revoked_at").
		First(&identity, "client = ?", session.Client).Error; err != nil {
		return err
	}
	if identity.RevokedAt != nil || identity.AgentKeyID != session.SSHEnrollmentKey {
		return errors.New("the agent enrollment has changed")
	}
	if rescuev1.RescueAction(session.Action) == grantSSHAction {
		helper, err := requireReadyHelper(tx, session.Client)
		if err != nil {
			return err
		}
		if !helper.SSHWindowSupported || helper.SSHEnrollmentKey != session.SSHEnrollmentKey || helper.SSHAccessPort == 0 {
			return errors.New("temporary SSH is unavailable on this node")
		}
		if helper.SSHAccessPort != session.SSHPort {
			return errors.New("temporary SSH port differs from the node's configured port")
		}
	}
	if session.SSHPreviousGrant == "" {
		return nil
	}
	match := tx.Model(&models.RescueSession{}).Where("id = ? AND client = ? AND action = ? AND ssh_enrollment_key = ?",
		session.SSHPreviousGrant, session.Client, int32(grantSSHAction), session.SSHEnrollmentKey)
	var target models.RescueSession
	if err := match.First(&target).Error; err != nil {
		return err
	}
	if rescuev1.RescueAction(session.Action) == revokeSSHAction && target.State == int32(commonv1.OperationState_OPERATION_STATE_QUEUED) {
		now := time.Now().UTC()
		return tx.Model(&target).Where("state = ?", target.State).Updates(map[string]any{
			"state": int32(commonv1.OperationState_OPERATION_STATE_CANCELLED), "finished_at": now, "updated_at": now,
			"error_code": "CANCELLED", "error_message": "temporary SSH authorization was withdrawn",
		}).Error
	}
	return nil
}

func findRescueAssignment(tx *gorm.DB, agentID string, helper models.ClientRescueHelper, row *models.RescueSession) error {
	queue := func() *gorm.DB {
		return tx.Where("client = ? AND state = ?", agentID, int32(commonv1.OperationState_OPERATION_STATE_QUEUED))
	}
	if helper.SSHWindowSupported && helper.SSHEnrollmentKey != "" {
		// Explicit revocations can queue while the helper is offline and
		// take precedence when it resumes, even if its port was reconfigured.
		err := queue().Where("action = ? AND ssh_enrollment_key = ? AND created_at > ?",
			int32(revokeSSHAction), helper.SSHEnrollmentKey, time.Now().UTC().Add(-sshWindowLifetime)).
			Order("created_at ASC").First(row).Error
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	queued := queue()
	if helper.SSHWindowSupported && helper.SSHAccessPort != 0 && helper.SSHEnrollmentKey != "" {
		// Bound delivery delay separately from the local 15-minute window.
		queued = queued.Where("(action NOT IN ? OR (action = ? AND ssh_enrollment_key = ? AND ssh_port = ? AND created_at > ?))",
			[]int32{int32(grantSSHAction), int32(revokeSSHAction)}, int32(grantSSHAction), helper.SSHEnrollmentKey,
			helper.SSHAccessPort, time.Now().UTC().Add(-defaultTimeout))
	} else {
		queued = queued.Where("action NOT IN ?", []int32{int32(grantSSHAction), int32(revokeSSHAction)})
	}
	return queued.Order("created_at ASC").First(row).Error
}

func recordSSHWindowStatus(tx *gorm.DB, agentID string, status *rescuev1.RescueHelperStatus, row *models.ClientRescueHelper) error {
	if !status.GetSshWindowSupported() {
		return nil
	}
	key := status.GetSshEnrollmentKeyId()
	if !sshEnrollmentKeyID.MatchString(key) || status.GetSshAccessPort() > 65535 {
		return errors.New("invalid temporary SSH helper status")
	}
	var identity models.AgentCredential
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("agent_key_id", "revoked_at").
		First(&identity, "client = ?", agentID).Error; err != nil {
		return err
	}
	if identity.RevokedAt != nil || identity.AgentKeyID != key {
		return errors.New("temporary SSH status belongs to a previous enrollment")
	}
	row.SSHWindowSupported, row.SSHAccessPort, row.SSHEnrollmentKey = true, status.GetSshAccessPort(), key
	access := status.GetSshAccess()
	if access == nil {
		return nil
	}
	if !sshGrantID.MatchString(access.GetGrantId()) || access.GetGrantedAt() == nil || !access.GetGrantedAt().IsValid() ||
		access.GetExpiresAt() == nil || !access.GetExpiresAt().IsValid() || access.GetPort() == 0 || access.GetPort() > 65535 {
		return errors.New("invalid temporary SSH window receipt")
	}
	var grant models.RescueSession
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&grant, "id = ? AND client = ? AND action = ?",
		access.GetGrantId(), agentID, int32(grantSSHAction)).Error; err != nil {
		return err
	}
	source, sourceErr := netip.ParseAddr(grant.SSHSourceIP)
	if grant.SSHEnrollmentKey != key || grant.SSHPort != access.GetPort() || sourceErr != nil || source.String() != access.GetSourceIp() ||
		access.GetExpiryMechanism() != rescuev1.ExpiryMechanism_EXPIRY_MECHANISM_NFTABLES_TIMEOUT ||
		!access.GetExpiresAt().AsTime().After(access.GetGrantedAt().AsTime()) ||
		access.GetExpiresAt().AsTime().Sub(access.GetGrantedAt().AsTime()) > sshWindowLifetime {
		return errors.New("temporary SSH receipt does not match its authorization")
	}
	var err error
	row.SSHAccess, err = proto.Marshal(access)
	if err != nil || !access.GetGranted() || grant.State != int32(commonv1.OperationState_OPERATION_STATE_RUNNING) ||
		grant.HelperInstanceID != row.HelperInstanceID {
		return err
	}
	// A periodic kernel receipt can finish a grant whose final RPC was lost.
	// A cancellation keeps its own state and the independently queued revoke.
	now := time.Now().UTC()
	succeeded := int32(commonv1.OperationState_OPERATION_STATE_SUCCEEDED)
	if err := tx.Create(&models.RescueEvent{
		Session: grant.ID, Sequence: 1, OccurredAt: now, State: succeeded, SSHAccess: row.SSHAccess,
	}).Error; err != nil {
		return err
	}
	return tx.Model(&grant).Where("state = ?", grant.State).
		Updates(map[string]any{"state": succeeded, "finished_at": now, "updated_at": now}).Error
}

func readSSHWindowStatus(db *gorm.DB, agentID string, row *models.ClientRescueHelper, status *rescuev1.RescueHelperStatus) error {
	if len(row.SSHAccess) == 0 {
		return nil
	}
	access := &rescuev1.TemporarySSHAccess{}
	if err := proto.Unmarshal(row.SSHAccess, access); err != nil {
		return err
	}
	status.SshAccess = access
	if !access.GetGranted() || !time.Now().Before(access.GetExpiresAt().AsTime()) {
		return nil
	}
	var count int64
	err := db.Model(&models.RescueSession{}).Where("client = ? AND ssh_previous_grant = ? AND action = ? AND state IN ?",
		agentID, access.GetGrantId(), int32(revokeSSHAction), []int32{
			int32(commonv1.OperationState_OPERATION_STATE_QUEUED), int32(commonv1.OperationState_OPERATION_STATE_RUNNING),
		}).Count(&count).Error
	status.SshRevokePending = count > 0
	return err
}
