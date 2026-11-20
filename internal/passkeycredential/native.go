package passkeycredential

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
)

func nativePolicy() (map[string]any, error) {
	origin := os.Getenv("KOMARI_PASSKEY_ORIGIN")
	rp := os.Getenv("KOMARI_PASSKEY_RP_ID")
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || rp == "" || (u.Hostname() != rp && !strings.HasSuffix(u.Hostname(), "."+rp)) {
		return nil, credential.ErrProviderUnavailable
	}
	synced := os.Getenv("KOMARI_PASSKEY_ALLOW_SYNCED")
	if synced != "" && synced != "false" && synced != "true" {
		return nil, credential.ErrProviderUnavailable
	}
	return map[string]any{"rp_id": rp, "origins": origin, "allow_synced_credentials": synced == "true"}, nil
}
func idDigest(id []byte) string { h := sha256.Sum256(id); return hex.EncodeToString(h[:]) }
func rowRecord(row models.UserPasskey) Record {
	return Record{UserUUID: row.UserUUID, ID: row.CredentialID, PublicKey: row.PublicKey, Transports: row.Transports, DeviceType: row.DeviceType, BackedUp: row.BackedUp}
}
func nativeSnapshot(ctx context.Context, user string) (Snapshot, error) {
	config, err := nativePolicy()
	if err != nil {
		return Snapshot{}, err
	}
	var rows []models.UserPasskey
	err = dbcore.GetDBInstance().WithContext(ctx).Where("user_uuid = ? AND rp_id = ?", user, config["rp_id"]).Order("credential_hash").Find(&rows).Error
	if err != nil {
		return Snapshot{}, err
	}
	records := make([]Record, len(rows))
	for i, row := range rows {
		records[i] = rowRecord(row)
	}
	// Ownership, policy and credential material invalidate grants. Sign counts,
	// last use and mutable backup state do not invalidate parallel ceremonies.
	stable := append([]Record(nil), records...)
	for i := range stable {
		stable[i].BackedUp = false
	}
	raw, err := json.Marshal([]any{"native:passkey", config, stable})
	if err != nil {
		return Snapshot{}, err
	}
	sum := sha256.Sum256(raw)
	return Snapshot{Revision: hex.EncodeToString(sum[:]), Running: true, Records: records, Config: config}, nil
}
func credentialKey(id string) (string, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(id)
	if err != nil || len(raw) == 0 || len(raw) > 1023 || base64.RawURLEncoding.EncodeToString(raw) != id {
		return "", errors.New("invalid credential ID")
	}
	return idDigest(raw), nil
}
