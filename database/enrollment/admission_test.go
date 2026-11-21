package enrollment

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
)

func TestEnrollmentAdmissionBoundsStorageAndKeepsDeviceSecretsPrivate(t *testing.T) {
	db, _, _, now := rotationFixture(t)
	require.NoError(t, db.AutoMigrate(&models.AgentEnrollment{}))
	key := []byte("synthetic-public-key")
	var wg sync.WaitGroup
	results := make(chan Attempt, 12)
	failures := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := beginDB(db, key, "key", 1, Attempt{RemoteIP: "192.0.2.1"}, now)
			results <- a
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	accepted, rejected := 0, 0
	for err := range failures {
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, ErrEnrollmentCapacity)
			rejected++
		}
	}
	require.Equal(t, maxEnrollmentAttemptsPerKey, accepted)
	require.Equal(t, 12-maxEnrollmentAttemptsPerKey, rejected)
	secrets := map[string]bool{}
	for a := range results {
		if a.DeviceCode != "" {
			require.False(t, secrets[a.DeviceCode], "public-key matching must never return someone else's device secret")
			secrets[a.DeviceCode] = true
		}
	}
	_, err := beginDB(db, key, "key", 1, Attempt{RemoteIP: "192.0.2.2"}, now)
	require.ErrorIs(t, err, ErrEnrollmentCapacity, "changing IP must not bypass key capacity")
	_, err = beginDB(db, key, "key", 1, Attempt{RemoteIP: "192.0.2.1"}, now.Add(attemptLifetime+time.Second))
	require.NoError(t, err, "expired pending work must release capacity")
	var count int64
	require.NoError(t, db.Model(&models.AgentEnrollment{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestEnrollmentAdmissionSourceAndGlobalCapacity(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprintf("global=%v", global), func(t *testing.T) {
			db, _, _, now := rotationFixture(t)
			require.NoError(t, db.AutoMigrate(&models.AgentEnrollment{}))
			limit := maxEnrollmentAttemptsPerIP
			if global {
				limit = maxEnrollmentAttempts
			}
			rows := make([]models.AgentEnrollment, limit)
			for i := range rows {
				source := "192.0.2.1"
				if global {
					source = fmt.Sprint(i)
				}
				rows[i] = models.AgentEnrollment{DeviceCode: fmt.Sprint(i), UserCode: fmt.Sprint(i), AgentPublicKey: fmt.Sprint(i), RemoteIP: source, State: StatePending, ExpiresAt: now.Add(time.Minute)}
			}
			require.NoError(t, db.CreateInBatches(rows, 50).Error)
			_, err := beginDB(db, []byte("new-key"), "key", 1, Attempt{RemoteIP: "192.0.2.1"}, now)
			require.ErrorIs(t, err, ErrEnrollmentCapacity)
			if !global {
				_, err = beginDB(db, []byte("other-key"), "key", 1, Attempt{RemoteIP: "192.0.2.2"}, now)
				require.NoError(t, err, "one source must not fill another source's pending quota")
			}
		})
	}
}

func TestHasAccessTokenRecordDistinguishesKnownFromUnknown(t *testing.T) {
	db, _, _, now := rotationFixture(t)
	const knownToken = "expired-agent-access-token"
	require.NoError(t, db.Create(&models.AgentCredential{
		Client:           "node-1",
		AccessTokenHash:  hashToken(knownToken),
		RefreshTokenHash: hashToken("refresh-1"),
		AccessExpiresAt:  now.Add(-time.Hour),
		RefreshExpiresAt: now.Add(time.Hour),
		AgentPublicKey:   "pub-1",
	}).Error)

	require.True(t, hasAccessTokenRecordDB(db, knownToken))
	require.False(t, hasAccessTokenRecordDB(db, "random-unknown-token"))
	require.False(t, hasAccessTokenRecordDB(db, ""))
}
