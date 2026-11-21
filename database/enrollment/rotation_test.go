package enrollment

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func rotationFixture(t *testing.T) (*gorm.DB, string, models.AgentCredential, time.Time) {
	t.Helper()
	t.Setenv("KOMARI_SECRET_KEY", "isolated-ci-key-not-a-real-secret")
	path := filepath.Join(t.TempDir(), "rotation.db") + "?_busy_timeout=3000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	pool.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = pool.Close() })
	require.NoError(t, db.AutoMigrate(&models.AgentCredential{}, &models.AgentRefreshUse{}, &models.AgentCredentialPolicy{}))
	now := time.Now().UTC().Truncate(time.Millisecond)
	token := "synthetic-refresh-" + uuid.NewString()
	row := models.AgentCredential{Client: uuid.NewString(), RefreshTokenHash: hashToken(token), AccessTokenHash: hashToken("synthetic-old-access"),
		AccessExpiresAt: now.Add(time.Hour), RefreshExpiresAt: now.Add(180 * 24 * time.Hour), AgentPublicKey: "synthetic-key", Scopes: "[]"}
	require.NoError(t, db.Create(&row).Error)
	return db, token, row, now
}

func TestCredentialRotationConcurrentSameRequestHasOneResult(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := uuid.NewString()
	var wg sync.WaitGroup
	results := make(chan Issued, 8)
	failures := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			issued, err := rotateVerifiedDB(ctx, db, record, token, id, nil, 2, func() time.Time { return now })
			results <- issued
			failures <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	var access string
	for issued := range results {
		if access == "" {
			access = issued.AccessToken
		}
		require.Equal(t, access, issued.AccessToken)
		require.Equal(t, uint64(1), issued.Generation)
	}
	var count int64
	require.NoError(t, db.Model(&models.AgentRefreshUse{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	var current models.AgentCredential
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.Equal(t, uint64(1), current.Generation)
	require.NotContains(t, current.RefreshRecoverySealed, token)
	require.NotContains(t, current.RefreshRecoverySealed, access)
}

func TestCredentialRotationRecoversAfterDatabaseReopen(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	id := uuid.NewString()
	first, err := rotateVerifiedDB(context.Background(), db, record, token, id, nil, 2, func() time.Time { return now })
	require.NoError(t, err)
	// A separate connection represents another panel process/restart, with no
	// process-local cache involved in response recovery.
	var filename string
	rows, err := db.Raw("PRAGMA database_list").Rows()
	require.NoError(t, err)
	for rows.Next() {
		var seq int
		var name string
		require.NoError(t, rows.Scan(&seq, &name, &filename))
		if name == "main" {
			break
		}
	}
	require.NoError(t, rows.Close())
	reopened, err := gorm.Open(sqlite.Open(filename), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	pool, err := reopened.DB()
	require.NoError(t, err)
	defer pool.Close()
	zero := uint64(0)
	recovered, err := rotateVerifiedDB(context.Background(), reopened, record, token, id, &zero, 2, func() time.Time { return now.Add(30 * time.Second) })
	require.NoError(t, err)
	require.Equal(t, first, recovered)
	_, err = rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), &zero, 2, func() time.Time { return now.Add(30 * time.Second) })
	require.ErrorIs(t, err, ErrRefreshConflict)
	var current models.AgentCredential
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.Nil(t, current.RevokedAt)
}

func TestCredentialRotationAuthenticatedReuseRevokesChain(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	id := uuid.NewString()
	_, err := rotateVerifiedDB(context.Background(), db, record, token, id, nil, 2, func() time.Time { return now })
	require.NoError(t, err)
	// Same request ID after recovery window must signal recovery-required without revoking the chain.
	_, err = rotateVerifiedDB(context.Background(), db, record, token, id, nil, 2, func() time.Time { return now.Add(RefreshRecoveryWindow) })
	require.ErrorIs(t, err, ErrRefreshRecoveryRequired)
	var unrevoked models.AgentCredential
	require.NoError(t, db.First(&unrevoked, "client = ?", record.Client).Error)
	require.Nil(t, unrevoked.RevokedAt)

	// Different request ID reusing a consumed refresh token must revoke the chain.
	_, err = rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now.Add(RefreshRecoveryWindow) })
	require.ErrorIs(t, err, ErrRefreshReuse)
	var current models.AgentCredential
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.NotNil(t, current.RevokedAt)
	require.Empty(t, current.RefreshRecoverySealed)
	_, err = rotateVerifiedDB(context.Background(), db, current, "unknown-token", uuid.NewString(), nil, 2, func() time.Time { return now })
	require.ErrorIs(t, err, ErrCredentialRejected)
}

func TestCredentialRotationSealFailureDoesNotConsumeAndRecoveryReplaysExactSignedBytes(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	id := uuid.NewString()
	_, _, err := rotateVerifiedDBWithSeal(context.Background(), db, record, token, id, nil, 2, func() time.Time { return now }, func(Issued) ([]byte, error) {
		return nil, errors.New("synthetic signing failure")
	})
	require.ErrorContains(t, err, "synthetic signing failure")
	var usedCount int64
	require.NoError(t, db.Model(&models.AgentRefreshUse{}).Count(&usedCount).Error)
	require.Zero(t, usedCount)

	sealCalls := 0
	expectedBytes := []byte("signed-envelope-bytes-v1")
	first, signedFirst, err := rotateVerifiedDBWithSeal(context.Background(), db, record, token, id, nil, 2, func() time.Time { return now }, func(Issued) ([]byte, error) {
		sealCalls++
		return expectedBytes, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, sealCalls)
	require.Equal(t, expectedBytes, signedFirst)

	// Retry while AccessToken is still fresh replays exact sealed bytes without re-signing.
	recovered, signedRecovered, err := rotateVerifiedDBWithSeal(context.Background(), db, record, token, id, nil, 2, func() time.Time { return now.Add(15 * time.Minute) }, func(Issued) ([]byte, error) {
		sealCalls++
		return []byte("should-not-resign"), nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, sealCalls)
	require.Equal(t, first, recovered)
	require.Equal(t, expectedBytes, signedRecovered)

	// Retry after the 1h AccessToken expires (within 24h recovery window) preserves
	// Generation & RefreshToken while issuing a fresh AccessToken and re-sealing.
	reSealedBytes := []byte("signed-envelope-bytes-refreshed-at")
	lateRecovered, lateSigned, err := rotateVerifiedDBWithSeal(context.Background(), db, record, token, id, nil, 2, func() time.Time { return now.Add(3 * time.Hour) }, func(candidate Issued) ([]byte, error) {
		sealCalls++
		require.Equal(t, first.Generation, candidate.Generation)
		require.Equal(t, first.RefreshToken, candidate.RefreshToken)
		require.NotEqual(t, first.AccessToken, candidate.AccessToken)
		require.True(t, candidate.AccessTokenExpiresAt.After(now.Add(3*time.Hour)))
		return reSealedBytes, nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, sealCalls)
	require.Equal(t, first.Generation, lateRecovered.Generation)
	require.Equal(t, first.RefreshToken, lateRecovered.RefreshToken)
	require.NotEqual(t, first.AccessToken, lateRecovered.AccessToken)
	require.Equal(t, reSealedBytes, lateSigned)
}

func TestCredentialRotationLegacyConsumedRowWithoutRequestHashDoesNotRevokePreviousGeneration(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	first, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.NoError(t, err)
	// Simulate a pre-upgrade AgentRefreshUse row that predates RequestHash recording.
	require.NoError(t, db.Model(&models.AgentRefreshUse{}).Where("client = ?", record.Client).Update("request_hash", "").Error)

	// Late retry of the immediately previous generation must not revoke the active chain.
	_, err = rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now.Add(RefreshRecoveryWindow + time.Minute) })
	require.ErrorIs(t, err, ErrRefreshRecoveryRequired)
	var current models.AgentCredential
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.Nil(t, current.RevokedAt)

	// Once rotated another generation forward, reusing the older pre-upgrade token still revokes the chain.
	gen1 := first.Generation
	_, err = rotateVerifiedDB(context.Background(), db, record, first.RefreshToken, uuid.NewString(), &gen1, 2, func() time.Time { return now.Add(RefreshRecoveryWindow + 2*time.Minute) })
	require.NoError(t, err)
	_, err = rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now.Add(RefreshRecoveryWindow + 3*time.Minute) })
	require.ErrorIs(t, err, ErrRefreshReuse)
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.NotNil(t, current.RevokedAt)
}

func TestCredentialRotationRejectsGenerationDowngradeAndChangedKey(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	wrong := uint64(9)
	_, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), &wrong, 2, func() time.Time { return now })
	require.ErrorIs(t, err, ErrRefreshConflict)
	require.NoError(t, db.Model(&models.AgentCredential{}).Where("client = ?", record.Client).Update("agent_public_key", "replacement-key").Error)
	_, err = rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.ErrorIs(t, err, ErrCredentialRejected)
	record.AgentPublicKey = "replacement-key"
	first, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.NoError(t, err)
	_, err = rotateVerifiedDB(context.Background(), db, record, first.RefreshToken, "legacy:"+first.RefreshToken, nil, 1, func() time.Time { return now })
	require.ErrorIs(t, err, ErrCredentialRejected)
	_, err = rotateVerifiedDB(context.Background(), db, record, first.RefreshToken, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.ErrorIs(t, err, ErrRefreshConflict)
}

func TestCredentialRotationRollbackAndExpiredAccessHandoff(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	expired := now.Add(-time.Second)
	require.NoError(t, db.Model(&record).Update("access_expires_at", expired).Error)
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("ci:rotation-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "agent_credentials" {
			tx.AddError(errors.New("synthetic DB write failure"))
		}
	}))
	_, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.ErrorContains(t, err, "synthetic DB write failure")
	require.NoError(t, db.Callback().Update().Remove("ci:rotation-failure"))
	var count int64
	require.NoError(t, db.Model(&models.AgentRefreshUse{}).Count(&count).Error)
	require.Zero(t, count)
	var unchanged models.AgentCredential
	require.NoError(t, db.First(&unchanged, "client = ?", record.Client).Error)
	require.Equal(t, record.RefreshTokenHash, unchanged.RefreshTokenHash)
	_, err = rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.NoError(t, err)
	var current models.AgentCredential
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.NotNil(t, current.PreviousAccessExpires)
	require.False(t, current.PreviousAccessExpires.After(expired))
}

func TestCredentialRotationLegacyRecoveryDoesNotRotateAgain(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	first, err := rotateVerifiedDB(context.Background(), db, record, token, "legacy:"+token, nil, 1, func() time.Time { return now })
	require.NoError(t, err)
	next, err := rotateVerifiedDB(context.Background(), db, record, token, "legacy:"+token, nil, 1, func() time.Time { return now.Add(time.Minute) })
	require.NoError(t, err)
	require.Equal(t, first, next)
	var current models.AgentCredential
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.Equal(t, uint64(1), current.Generation)
}
