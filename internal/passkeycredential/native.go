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

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
)

func normalizeOriginEntry(raw, rp string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" || (hostname != rp && !strings.HasSuffix(hostname, "."+rp)) {
		return "", false
	}
	return "https://" + strings.ToLower(u.Host), true
}

func validRPID(rp string) bool {
	if rp == "" || len(rp) > 253 || strings.HasPrefix(rp, ".") || strings.HasSuffix(rp, ".") {
		return false
	}
	if strings.ContainsAny(rp, " \t\r\n/:?#[]@") {
		return false
	}
	return true
}

func nativePolicy() (map[string]any, error) {
	rp := strings.ToLower(strings.TrimSpace(os.Getenv("KOMARI_PASSKEY_RP_ID")))
	if !validRPID(rp) {
		return nil, credential.ErrProviderUnavailable
	}
	rawOrigins := strings.TrimSpace(os.Getenv("KOMARI_PASSKEY_ORIGIN"))
	panelOrigins := strings.TrimSpace(os.Getenv("KOMARI_PASSKEY_PANEL_ORIGIN"))
	effectivePanelOrigin := flags.EffectivePanelOrigin()
	combined := rawOrigins
	if panelOrigins != "" {
		if combined != "" {
			combined += "," + panelOrigins
		} else {
			combined = panelOrigins
		}
	}
	if combined == "" && effectivePanelOrigin != "" {
		combined = effectivePanelOrigin
	}
	if combined == "" {
		return nil, credential.ErrProviderUnavailable
	}
	parts := strings.FieldsFunc(combined, func(r rune) bool { return r == ',' || r == '\r' || r == '\n' })
	normalized := make([]string, 0, len(parts)+1)
	seen := make(map[string]bool, len(parts)+1)
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		entry, ok := normalizeOriginEntry(part, rp)
		if !ok {
			return nil, credential.ErrProviderUnavailable
		}
		if !seen[entry] {
			seen[entry] = true
			normalized = append(normalized, entry)
		}
	}
	if effectivePanelOrigin != "" {
		if entry, ok := normalizeOriginEntry(effectivePanelOrigin, rp); ok && !seen[entry] {
			seen[entry] = true
			normalized = append(normalized, entry)
		}
	}
	if len(normalized) == 0 {
		return nil, credential.ErrProviderUnavailable
	}
	synced := strings.TrimSpace(os.Getenv("KOMARI_PASSKEY_ALLOW_SYNCED"))
	if synced != "" && synced != "false" && synced != "true" {
		return nil, credential.ErrProviderUnavailable
	}
	return map[string]any{
		"rp_id":                    rp,
		"origins":                  strings.Join(normalized, ","),
		"allow_synced_credentials": synced == "true",
	}, nil
}

// ActivePolicy returns the active native RP policy when valid native Passkey
// settings are configured.
func ActivePolicy() (rpID string, allowSynced bool, ok bool) {
	cfg, err := nativePolicy()
	if err != nil {
		return "", false, false
	}
	rp, _ := cfg["rp_id"].(string)
	synced, _ := cfg["allow_synced_credentials"].(bool)
	return rp, synced, rp != ""
}

func idDigest(id []byte) string { h := sha256.Sum256(id); return hex.EncodeToString(h[:]) }
func rowRecord(row models.UserPasskey) Record {
	return Record{UserUUID: row.UserUUID, ID: row.CredentialID, PublicKey: row.PublicKey, Transports: row.Transports, DeviceType: row.DeviceType, BackedUp: row.BackedUp}
}
func nativeSnapshot(ctx context.Context, user string) (Snapshot, error) {
	config, policyErr := nativePolicy()
	db := dbcore.GetDBInstance().WithContext(ctx)
	var rows []models.UserPasskey
	query := db.Where("user_uuid = ?", user)
	if policyErr == nil {
		query = query.Where("rp_id = ?", config["rp_id"])
	}
	if err := query.Order("credential_hash").Find(&rows).Error; err != nil {
		return Snapshot{}, err
	}
	if len(rows) > 0 {
		var revokedHashes []string
		if err := db.Model(&models.PasskeyRevocation{}).
			Where("user_uuid = ?", user).
			Pluck("credential_hash", &revokedHashes).Error; err != nil {
			return Snapshot{}, credential.ErrProviderUnavailable
		}
		if len(revokedHashes) > 0 {
			revokedSet := make(map[string]bool, len(revokedHashes))
			for _, h := range revokedHashes {
				revokedSet[h] = true
			}
			filtered := rows[:0]
			for _, row := range rows {
				if !revokedSet[row.CredentialHash] {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}
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
	if policyErr != nil {
		// Fail soft for non-Passkey backup factors: keep Running=false so
		// Passkey is unavailable, while preserving Records so enrolled Passkey
		// accounts cannot downgrade past second-factor checks.
		raw, err := json.Marshal([]any{
			"native:passkey:misconfigured",
			os.Getenv("KOMARI_PASSKEY_RP_ID"),
			os.Getenv("KOMARI_PASSKEY_ORIGIN"),
			os.Getenv("KOMARI_PASSKEY_PANEL_ORIGIN"),
			flags.EffectivePanelOrigin(),
			os.Getenv("KOMARI_PASSKEY_ALLOW_SYNCED"),
			stable,
		})
		if err != nil {
			return Snapshot{}, err
		}
		sum := sha256.Sum256(raw)
		return Snapshot{Revision: hex.EncodeToString(sum[:]), Running: false, Records: records, Config: nil}, nil
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
