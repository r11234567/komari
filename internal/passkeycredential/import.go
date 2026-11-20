package passkeycredential

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ImportReport struct {
	Eligible       int
	Conflicts      int
	Rebind         int
	AlreadyApplied bool
}

func validPublicRecord(r Record, synced bool) ([]byte, bool) {
	key, err := base64.RawURLEncoding.Strict().DecodeString(r.PublicKey)
	if err != nil || len(key) > 4096 || base64.RawURLEncoding.EncodeToString(key) != r.PublicKey || r.UserUUID == "" || len(r.Transports) > 8 {
		return nil, false
	}
	switch r.DeviceType {
	case "singleDevice":
		if r.BackedUp {
			return nil, false
		}
	case "multiDevice":
		if !synced {
			return nil, false
		}
	default:
		return nil, false
	}
	for _, transport := range r.Transports {
		switch transport {
		case "usb", "nfc", "ble", "internal", "hybrid", "smart-card", "cable":
		default:
			return nil, false
		}
	}
	_, err = webauthncose.ParsePublicKey(key)
	return key, err == nil
}

// Import reads only the v1 public credential records. The caller opens a
// read-only database for preview; applying requires an offline primary DB.
func Import(ctx context.Context, db *gorm.DB, input io.Reader, apply bool) (ImportReport, error) {
	var report ImportReport
	cfg, err := nativePolicy()
	if err != nil {
		return report, err
	}
	rp := cfg["rp_id"].(string)
	raw, err := io.ReadAll(io.LimitReader(input, maxStoreBytes+1))
	if err != nil {
		return report, err
	}
	var doc struct {
		Version     int
		Credentials []Record
	}
	if len(raw) > maxStoreBytes || json.Unmarshal(raw, &doc) != nil || doc.Version != 1 || doc.Credentials == nil || len(doc.Credentials) > 1024 {
		return report, errors.New("invalid v1 credential store")
	}
	digest := sha256.Sum256(raw)
	mark := models.PasskeyImport{Source: pluginID, Digest: hex.EncodeToString(digest[:]), RPID: rp}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var prior models.PasskeyImport
		err := tx.Where("source = ?", pluginID).Take(&prior).Error
		if err == nil {
			if prior.RPID != rp || prior.Digest != mark.Digest {
				return errors.New("import source or RP changed; reconcile before retrying")
			}
			report.AlreadyApplied = true
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		frequency := make(map[string]int, len(doc.Credentials))
		for _, record := range doc.Credentials {
			frequency[record.ID]++
		}
		userCounts := make(map[string]int64)
		hasRevocationTable := tx.Migrator().HasTable(&models.PasskeyRevocation{})
		for _, r := range doc.Credentials {
			pk, err := credentialKey(r.ID)
			if err != nil {
				report.Rebind++
				continue
			}
			if frequency[r.ID] != 1 {
				report.Conflicts++
				continue
			}
			key, valid := validPublicRecord(r, cfg["allow_synced_credentials"] == true)
			if !valid {
				report.Rebind++
				continue
			}
			var user models.User
			err = tx.Select("uuid").Where("uuid = ?", r.UserUUID).Take(&user).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				report.Rebind++
				continue
			}
			if err != nil {
				return err
			}
			if hasRevocationTable {
				var revoked int64
				if err := tx.Model(&models.PasskeyRevocation{}).Where("credential_hash = ?", pk).Count(&revoked).Error; err != nil {
					return err
				}
				if revoked > 0 {
					report.Conflicts++
					continue
				}
			}
			if _, ok := userCounts[r.UserUUID]; !ok {
				var existing int64
				if err := tx.Model(&models.UserPasskey{}).Where("user_uuid = ? AND rp_id = ?", r.UserUUID, rp).Count(&existing).Error; err != nil {
					return err
				}
				userCounts[r.UserUUID] = existing
			}
			var row models.UserPasskey
			err = tx.Where("credential_hash = ?", pk).Take(&row).Error
			if err == nil {
				if row.UserUUID != r.UserUUID || row.CredentialID != r.ID || row.PublicKey != r.PublicKey || row.RPID != rp || row.DeviceType != r.DeviceType {
					report.Conflicts++
					continue
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			} else {
				if userCounts[r.UserUUID] >= 64 {
					report.Conflicts++
					continue
				}
				userCounts[r.UserUUID]++
				if apply {
					row = models.UserPasskey{CredentialHash: pk, UserUUID: r.UserUUID, RPID: rp, CredentialID: r.ID, PublicKey: r.PublicKey, Transports: r.Transports, DeviceType: r.DeviceType, BackedUp: r.BackedUp, Name: "Imported passkey"}
					if err = tx.Omit("User").Create(&row).Error; err != nil {
						return err
					}
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
			report.Eligible++
		}
		if apply {
			if report.Conflicts+report.Rebind > 0 {
				return errors.New("import needs reconciliation; transaction rolled back")
			}
			return tx.Create(&mark).Error
		}
		return nil
	})
	return report, err
}
