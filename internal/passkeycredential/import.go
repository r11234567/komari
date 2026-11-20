package passkeycredential

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Import reads only the v1 public credential records. The caller opens a
// read-only database for preview; applying requires an offline primary DB.
func Import(ctx context.Context, db *gorm.DB, input io.Reader, apply bool) (int, error) {
	cfg, err := nativePolicy()
	if err != nil {
		return 0, err
	}
	rp := cfg["rp_id"].(string)
	raw, err := io.ReadAll(io.LimitReader(input, maxStoreBytes+1))
	if err != nil {
		return 0, err
	}
	var doc struct {
		Version     int
		Credentials []Record
	}
	if len(raw) > maxStoreBytes || json.Unmarshal(raw, &doc) != nil || doc.Version != 1 || doc.Credentials == nil || len(doc.Credentials) > 1024 {
		return 0, errors.New("invalid v1 credential store")
	}
	digest := sha256.Sum256(raw)
	mark := models.PasskeyImport{Source: pluginID, Digest: hex.EncodeToString(digest[:]), RPID: rp}
	count := 0
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var prior models.PasskeyImport
		err := tx.Where("source = ?", pluginID).Take(&prior).Error
		if err == nil {
			if prior.RPID != rp || prior.Digest != mark.Digest {
				return errors.New("import source or RP changed; reconcile before retrying")
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		seen := map[string]bool{}
		for i, r := range doc.Credentials {
			pk, err := credentialKey(r.ID)
			key, ke := base64.RawURLEncoding.Strict().DecodeString(r.PublicKey)
			if err != nil || ke != nil || len(key) > 4096 || seen[pk] || r.UserUUID == "" || len(r.Transports) > 8 || (r.DeviceType != "singleDevice" && r.DeviceType != "multiDevice") || (r.BackedUp && r.DeviceType != "multiDevice") {
				return fmt.Errorf("invalid or duplicate credential at record %d", i+1)
			}
			if r.DeviceType == "multiDevice" && cfg["allow_synced_credentials"] != true {
				return fmt.Errorf("synced credential requires re-enrollment or an approved synced policy at record %d", i+1)
			}
			for _, transport := range r.Transports {
				switch transport {
				case "usb", "nfc", "ble", "internal", "hybrid", "smart-card", "cable":
				default:
					return fmt.Errorf("invalid transport at record %d", i+1)
				}
			}
			if _, err = webauthncose.ParsePublicKey(key); err != nil {
				return fmt.Errorf("invalid public key at record %d", i+1)
			}
			seen[pk] = true
			var user models.User
			if err = tx.Select("uuid").Where("uuid = ?", r.UserUUID).Take(&user).Error; err != nil || user.UUID != r.UserUUID {
				return fmt.Errorf("unknown owner at record %d", i+1)
			}
			var row models.UserPasskey
			err = tx.Where("credential_hash = ?", pk).Take(&row).Error
			if err == nil {
				if row.UserUUID != r.UserUUID || row.CredentialID != r.ID || row.PublicKey != r.PublicKey || row.RPID != rp || row.DeviceType != r.DeviceType {
					return fmt.Errorf("credential conflict at record %d", i+1)
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			} else if apply {
				row = models.UserPasskey{CredentialHash: pk, UserUUID: r.UserUUID, RPID: rp, CredentialID: r.ID, PublicKey: r.PublicKey, Transports: r.Transports, DeviceType: r.DeviceType, BackedUp: r.BackedUp, Name: "Imported passkey"}
				if err = tx.Omit("User").Create(&row).Error; err != nil {
					return err
				}
			}
			if apply {
				id, _ := base64.RawURLEncoding.DecodeString(r.ID)
				high := models.PasskeyCounter{UserUUID: r.UserUUID, KeyDigest: keyDigest(id, key), Counter: r.Counter}
				if err = tx.Omit("User").Clauses(clause.OnConflict{DoNothing: true}).Create(&high).Error; err != nil {
					return err
				}
				if err = tx.Model(&high).Where("counter < ?", r.Counter).Update("counter", r.Counter).Error; err != nil {
					return err
				}
			}
			count++
		}
		if apply {
			return tx.Create(&mark).Error
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
