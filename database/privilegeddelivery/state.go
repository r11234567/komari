package privilegeddelivery

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// settingsHash uses privileged-settings-v1, specified in komari-proto/docs.
// Fixed positions normalize optional false and absent to the same setting.
func settingsHash(s Settings) string {
	b := []byte("komari.privileged-settings.v1\x00")
	for _, v := range []bool{s.RemoteControlEnabled, s.WebSSHEnabled, s.ExecutionEnabled, s.EnableGPU, s.RescueHelperEnabled} {
		if v {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
	}
	b = binary.BigEndian.AppendUint32(b, uint32(s.RequiredPrivilegeMode))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func lockState(tx *gorm.DB, id string) (models.ClientPrivilegedState, error) {
	if strings.TrimSpace(id) == "" {
		return models.ClientPrivilegedState{}, errors.New("agent ID is required")
	}
	var client models.Client
	if err := tx.Select("uuid").First(&client, "uuid = ?", id).Error; err != nil {
		return models.ClientPrivilegedState{}, err
	}
	var latest models.ClientPrivilegedRevision
	err := tx.Where("client = ?", id).Order("revision DESC").First(&latest).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.ClientPrivilegedState{}, err
	}
	initial := models.ClientPrivilegedState{Client: id, DesiredRevision: latest.Revision, ProtocolVersion: 1}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
		return initial, err
	}
	var state models.ClientPrivilegedState
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&state, "client = ?", id).Error
	return state, err
}

func advance(tx *gorm.DB, state *models.ClientPrivilegedState, row *models.ClientPrivilegedRevision) error {
	state.UpdateSequence++
	state.UpdatedAt = time.Now().UTC()
	row.UpdateSequence = state.UpdateSequence
	row.UpdatedAt = state.UpdatedAt
	if err := tx.Save(row).Error; err != nil {
		return err
	}
	return tx.Save(state).Error
}

type Snapshot struct {
	Desired, Applied *Revision
	State            models.ClientPrivilegedState
}

func GetState(id string) (Snapshot, error) { return GetStateWithDB(dbcore.GetDBInstance(), id) }
func GetStateWithDB(db *gorm.DB, id string) (Snapshot, error) {
	var out Snapshot
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, id)
		if err != nil {
			return err
		}
		out.State = state
		for _, entry := range []struct {
			n      uint64
			target **Revision
		}{{state.DesiredRevision, &out.Desired}, {state.AppliedRevision, &out.Applied}} {
			if entry.n == 0 {
				continue
			}
			r, err := lockRevision(tx, id, entry.n)
			if err != nil {
				return err
			}
			v := toRevision(r)
			*entry.target = &v
		}
		return nil
	})
	return out, err
}

func Negotiate(db *gorm.DB, id, service, startup string, mode int32, actions []int32) (models.ClientPrivilegedState, error) {
	var out models.ClientPrivilegedState
	if strings.TrimSpace(service) == "" || len(service) > 255 || strings.TrimSpace(startup) == "" || len(startup) > 128 || mode < 1 || mode > 5 {
		return out, errors.New("valid service, startup ID and actual mode are required")
	}
	for _, a := range actions {
		if a < 1 || a > 3 {
			return out, errors.New("unsupported host action")
		}
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		state, err := lockState(tx, id)
		if err != nil {
			return err
		}
		if state.ServiceID != "" && state.ServiceID != service {
			return errors.New("service identity binding cannot be changed by negotiation")
		}
		data, _ := json.Marshal(actions)
		state.ProtocolVersion = 2
		state.ServiceID = service
		state.StartupID = startup
		state.PrivilegeMode = mode
		state.SupportedActions = string(data)
		state.UpdateSequence++
		state.UpdatedAt = time.Now().UTC()
		if err := tx.Save(&state).Error; err != nil {
			return err
		}
		out = state
		return nil
	})
	return out, err
}

func requiredActions(tx *gorm.DB, state models.ClientPrivilegedState, row models.ClientPrivilegedRevision) ([]int32, error) {
	var actions []int32
	if row.ToPrivilegeMode != state.PrivilegeMode {
		actions = append(actions, 1)
	}
	settings := decodeSettings(row.Config)
	installed := false
	if state.Observation != "" {
		var o hostv1.DaemonObservation
		if err := protojson.Unmarshal([]byte(state.Observation), &o); err != nil {
			return nil, err
		}
		installed = o.HelperInstalled
	}
	if settings.RescueHelperEnabled && !installed {
		actions = append(actions, 2)
	}
	if !settings.RescueHelperEnabled && installed {
		actions = append(actions, 3)
	}
	return actions, nil
}
func supportsActions(state models.ClientPrivilegedState, required []int32) bool {
	var supported []int32
	if json.Unmarshal([]byte(state.SupportedActions), &supported) != nil {
		return len(required) == 0
	}
	for _, r := range required {
		found := false
		for _, a := range supported {
			if a == r {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func receipt(tx *gorm.DB, id, key string, payload proto.Message) (bool, error) {
	if strings.TrimSpace(key) == "" || len(key) > 128 {
		return false, errors.New("receipt ID is required (max 128 bytes)")
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	var old models.ClientPrivilegedReceipt
	err = tx.First(&old, "client = ? AND receipt_id = ?", id, key).Error
	if err == nil {
		if old.PayloadHash != hash {
			return false, errors.New("receipt ID reused with different content")
		}
		return true, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	return false, tx.Create(&models.ClientPrivilegedReceipt{Client: id, ReceiptID: key, PayloadHash: hash, CreatedAt: time.Now().UTC()}).Error
}

func validateCurrent(state models.ClientPrivilegedState, row models.ClientPrivilegedRevision) error {
	if row.ProtocolVersion != 2 || state.ProtocolVersion != 2 {
		return errors.New("privileged_delivery_v2 is required")
	}
	if state.DesiredRevision != row.Revision || row.SupersededAt != nil || row.State == StateSuperseded {
		return errors.New("revision has been superseded")
	}
	if row.ConfirmedAt == nil || row.ApprovalGeneration == 0 {
		return errors.New("revision is awaiting panel confirmation")
	}
	return nil
}

// Conflict provides a structured revision hint without claiming a failed 2FA.
func Conflict(id string, err error) error {
	return fmt.Errorf("%w (refresh current state for agent %s)", err, id)
}
