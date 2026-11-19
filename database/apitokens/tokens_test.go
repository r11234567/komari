package apitokens

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func tokenDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tokens.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.Client{}, &models.APIToken{}))
	require.NoError(t, db.Create(&models.User{UUID: "owner", Username: "owner"}).Error)
	require.NoError(t, db.Create(&models.Client{UUID: "node-a"}).Error)
	return db
}

func TestTokenSecretScopeExpiryRevocationAndAtomicRotation(t *testing.T) {
	db := tokenDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	policy := Policy{Name: "reader", Scopes: []string{rpc.ScopeNodesRead}, AgentIDs: []string{"node-a"}, AllowedCIDRs: []string{"192.0.2.8/32"}, ExpiresAt: now.Add(time.Hour)}
	row, secret, err := create(ctx, db, "owner", policy, "", now)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(secret, Prefix))
	require.NotContains(t, row.SecretHash, secret)
	serialized, err := json.Marshal(row)
	require.NoError(t, err)
	require.NotContains(t, string(serialized), row.SecretHash)
	p, err := authenticate(ctx, db, secret, "192.0.2.8", now)
	require.NoError(t, err)
	require.Empty(t, p.UserUUID)
	require.False(t, p.HasRole(rpc.RoleAdmin))
	require.True(t, p.AllowsAgent("node-a"))
	require.False(t, p.AllowsAgent("node-b"))
	_, err = authenticate(ctx, db, secret, "192.0.2.9", now)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = authenticate(ctx, db, secret+"x", "192.0.2.8", now)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = authenticate(ctx, db, secret, "192.0.2.8", now.Add(2*time.Hour))
	require.ErrorIs(t, err, ErrInvalid)
	// A failed rotation must not revoke the still-used original token.
	_, _, err = create(ctx, db, "owner", Policy{Name: "bad", Scopes: policy.Scopes, AgentIDs: []string{"missing"}, ExpiresAt: policy.ExpiresAt}, row.ID, now)
	require.Error(t, err)
	_, err = authenticate(ctx, db, secret, "192.0.2.8", now)
	require.NoError(t, err)
	_, next, err := create(ctx, db, "owner", policy, row.ID, now)
	require.NoError(t, err)
	_, err = authenticate(ctx, db, secret, "192.0.2.8", now)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = authenticate(ctx, db, next, "192.0.2.8", now)
	require.NoError(t, err)
	require.NoError(t, db.Where("uuid = ?", "owner").Delete(&models.User{}).Error)
	_, err = authenticate(ctx, db, next, "192.0.2.8", now)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestTokenPolicyRejectsBroadDefaultsUnknownScopesAndInfiniteLifetimes(t *testing.T) {
	now := time.Now().UTC()
	valid := Policy{Name: "read", Scopes: []string{rpc.ScopeNodesRead}, AgentIDs: []string{"node-a"}, ExpiresAt: now.Add(time.Hour)}
	for _, alter := range []func(*Policy){
		func(p *Policy) { p.Scopes = nil }, func(p *Policy) { p.Scopes = []string{"admin:*"} },
		func(p *Policy) { p.AgentIDs = nil }, func(p *Policy) { p.AllAgents = true },
		func(p *Policy) { p.ExpiresAt = time.Time{} }, func(p *Policy) { p.ExpiresAt = now.Add(MaxLifetime + time.Hour) },
		func(p *Policy) { p.AllowedCIDRs = []string{"*"} },
	} {
		p := valid
		alter(&p)
		_, err := Validate(p, now)
		require.ErrorIs(t, err, ErrPolicy)
	}
}
