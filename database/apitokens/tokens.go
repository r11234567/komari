package apitokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"gorm.io/gorm"
)

const Prefix = "komari_pat_"
const PageSize = 25
const MaxActiveTokens = 50
const MaxLifetime = 365 * 24 * time.Hour

var ErrInvalid = errors.New("invalid or expired API token")
var ErrPolicy = errors.New("invalid API token policy")

type Policy struct {
	Name                           string
	Scopes, AgentIDs, AllowedCIDRs []string
	AllAgents                      bool
	ExpiresAt                      time.Time
}

func Validate(policy Policy, now time.Time) (Policy, error) {
	policy.Scopes = slices.Clone(policy.Scopes)
	policy.AgentIDs = slices.Clone(policy.AgentIDs)
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	policy.Name = strings.TrimSpace(policy.Name)
	if policy.Name == "" || len(policy.Name) > 80 || !policy.ExpiresAt.After(now) || policy.ExpiresAt.After(now.Add(MaxLifetime)) {
		return policy, fmt.Errorf("%w: name and expiry within one year are required", ErrPolicy)
	}
	if len(policy.Scopes) == 0 || len(policy.Scopes) > 2 || len(policy.AgentIDs) > 500 || len(policy.AllowedCIDRs) > 16 {
		return policy, ErrPolicy
	}
	for _, scope := range policy.Scopes {
		if scope != rpc.ScopeNodesRead && scope != rpc.ScopeMetricsRead {
			return policy, ErrPolicy
		}
	}
	slices.Sort(policy.Scopes)
	policy.Scopes = slices.Compact(policy.Scopes)
	if policy.AllAgents == (len(policy.AgentIDs) > 0) {
		return policy, fmt.Errorf("%w: select explicit nodes or all nodes", ErrPolicy)
	}
	for _, id := range policy.AgentIDs {
		if strings.TrimSpace(id) != id || id == "" || len(id) > 64 {
			return policy, ErrPolicy
		}
	}
	slices.Sort(policy.AgentIDs)
	policy.AgentIDs = slices.Compact(policy.AgentIDs)
	for i, cidr := range policy.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.Addr().Is4In6() {
			return policy, fmt.Errorf("%w: invalid source CIDR", ErrPolicy)
		}
		policy.AllowedCIDRs[i] = prefix.Masked().String()
	}
	slices.Sort(policy.AllowedCIDRs)
	policy.AllowedCIDRs = slices.Compact(policy.AllowedCIDRs)
	return policy, nil
}

func Create(ctx context.Context, owner string, policy Policy, replaces string) (models.APIToken, string, error) {
	return create(ctx, dbcore.GetDBInstance(), owner, policy, replaces, time.Now().UTC())
}

func create(ctx context.Context, db *gorm.DB, owner string, policy Policy, replaces string, now time.Time) (models.APIToken, string, error) {
	var row models.APIToken
	policy, err := Validate(policy, now)
	if err != nil || owner == "" {
		if err == nil {
			err = ErrPolicy
		}
		return row, "", err
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return row, "", err
	}
	id := uuid.NewString()
	secret := Prefix + id + "_" + base64.RawURLEncoding.EncodeToString(secretBytes)
	digest := sha256.Sum256([]byte(secret))
	encode := func(values []string) string {
		if values == nil {
			values = []string{}
		}
		data, _ := json.Marshal(values)
		return string(data)
	}
	row = models.APIToken{ID: id, OwnerUUID: owner, Name: policy.Name, SecretHash: hex.EncodeToString(digest[:]), Scopes: encode(policy.Scopes), AgentIDs: encode(policy.AgentIDs), AllAgents: policy.AllAgents, AllowedCIDRs: encode(policy.AllowedCIDRs), CreatedAt: now, ExpiresAt: policy.ExpiresAt.UTC()}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialize token admission and rotation for one existing human owner.
		var user models.User
		if err := tx.Select("uuid").Where("uuid = ?", owner).First(&user).Error; err != nil {
			return err
		}
		result := tx.Model(&models.User{}).Where("uuid = ?", owner).UpdateColumn("uuid", owner)
		if result.Error != nil {
			return result.Error
		}
		if replaces != "" {
			result := tx.Model(&models.APIToken{}).Where("id = ? AND owner_uuid = ? AND revoked_at IS NULL", replaces, owner).Update("revoked_at", now)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrInvalid
			}
		}
		var count int64
		if err := tx.Model(&models.APIToken{}).Where("owner_uuid = ? AND revoked_at IS NULL AND expires_at > ?", owner, now).Count(&count).Error; err != nil {
			return err
		}
		if count >= MaxActiveTokens {
			return fmt.Errorf("at most %d active API tokens are permitted", MaxActiveTokens)
		}
		if len(policy.AgentIDs) > 0 {
			var nodes int64
			if err := tx.Model(&models.Client{}).Where("uuid IN ?", policy.AgentIDs).Count(&nodes).Error; err != nil {
				return err
			}
			if nodes != int64(len(policy.AgentIDs)) {
				return fmt.Errorf("%w: unknown node", ErrPolicy)
			}
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return models.APIToken{}, "", err
	}
	return row, secret, nil
}

func Authenticate(ctx context.Context, secret, remoteIP string) (*rpc.Principal, error) {
	return authenticate(ctx, dbcore.GetDBInstance(), secret, remoteIP, time.Now().UTC())
}

func authenticate(ctx context.Context, db *gorm.DB, secret, remoteIP string, now time.Time) (*rpc.Principal, error) {
	if len(secret) != len(Prefix)+36+1+43 || !strings.HasPrefix(secret, Prefix) || secret[len(Prefix)+36] != '_' {
		return nil, ErrInvalid
	}
	id := secret[len(Prefix) : len(Prefix)+36]
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrInvalid
	}
	var row models.APIToken
	if err := db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalid
		}
		return nil, err
	}
	digest := sha256.Sum256([]byte(secret))
	stored, err := hex.DecodeString(row.SecretHash)
	if err != nil || subtle.ConstantTimeCompare(digest[:], stored) != 1 || row.RevokedAt != nil || !now.Before(row.ExpiresAt) {
		return nil, ErrInvalid
	}
	var scopes, agents, cidrs []string
	if json.Unmarshal([]byte(row.Scopes), &scopes) != nil || json.Unmarshal([]byte(row.AgentIDs), &agents) != nil || json.Unmarshal([]byte(row.AllowedCIDRs), &cidrs) != nil {
		return nil, ErrInvalid
	}
	// Revalidate persisted policy; malformed data must not become broad access.
	if _, err := Validate(Policy{Name: row.Name, Scopes: slices.Clone(scopes), AgentIDs: slices.Clone(agents), AllAgents: row.AllAgents, AllowedCIDRs: slices.Clone(cidrs), ExpiresAt: row.ExpiresAt}, row.CreatedAt); err != nil {
		return nil, ErrInvalid
	}
	if len(cidrs) > 0 {
		ip, err := netip.ParseAddr(remoteIP)
		if err != nil {
			return nil, ErrInvalid
		}
		ip = ip.Unmap()
		allowed := false
		for _, raw := range cidrs {
			p, err := netip.ParsePrefix(raw)
			if err == nil && p.Contains(ip) {
				allowed = true
			}
		}
		if !allowed {
			return nil, ErrInvalid
		}
	}
	// A deleted/disabled account cannot leave a detached credential behind.
	var owner models.User
	if err := db.WithContext(ctx).Select("uuid").Where("uuid = ?", row.OwnerUUID).First(&owner).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalid
		}
		return nil, err
	}
	// Throttle metadata writes; authentication itself always reads revocation.
	if err := db.WithContext(ctx).Model(&models.APIToken{}).Where("id = ? AND revoked_at IS NULL AND (last_used_at IS NULL OR last_used_at < ?)", id, now.Add(-time.Minute)).Update("last_used_at", now).Error; err != nil {
		return nil, err
	}
	return &rpc.Principal{Type: rpc.PrincipalAPIKey, IsAPIKey: true, Roles: []string{rpc.RoleGuest}, APIToken: &rpc.APITokenPolicy{ID: row.ID, OwnerUUID: row.OwnerUUID, Scopes: scopes, AgentIDs: agents, AllAgents: row.AllAgents, ExpiresAt: row.ExpiresAt}}, nil
}

func List(ctx context.Context, owner string, page int) ([]models.APIToken, int64, error) {
	if page < 1 {
		page = 1
	}
	db := dbcore.GetDBInstance().WithContext(ctx).Model(&models.APIToken{}).Where("owner_uuid = ?", owner)
	var count int64
	if err := db.Count(&count).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.APIToken
	err := db.Order("created_at DESC, id DESC").Limit(PageSize).Offset((page - 1) * PageSize).Find(&rows).Error
	return rows, count, err
}

func Revoke(ctx context.Context, owner, id string) error {
	result := dbcore.GetDBInstance().WithContext(ctx).Model(&models.APIToken{}).Where("owner_uuid = ? AND id = ?", owner, id).Update("revoked_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrInvalid
	}
	return nil
}
