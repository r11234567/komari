package historyexport

import (
	"context"
	"sync"
	"time"
)

// Logical process I/O includes SQLite and cached reads. Observe at bounded,
// boundaries without holding a live-source write transaction. The private
// sampling spool may keep its own transaction. This is not a kernel I/O cap.
type ioBudget struct {
	mu    sync.Mutex
	rate  int64
	at    time.Time
	bytes uint64

	observationMu sync.Mutex
	waitStarted   time.Time
	waitDuration  time.Duration
}

func (b *ioBudget) wait(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Every cooperative boundary must account for cached reads too. A time-only
	// sampling shortcut lets fast cached reads accumulate megabytes in 100 ms,
	// followed by minutes of budget debt with no progress at the next boundary.
	n, e := processIOBytes()
	if e != nil {
		return e
	}
	now := time.Now()
	if b.at.IsZero() || n < b.bytes {
		b.at = now
		b.bytes = n
		return ctx.Err()
	}
	elapsed := now.Sub(b.at)
	required := time.Duration(float64(n-b.bytes) / float64(b.rate) * float64(time.Second))
	if required > elapsed {
		started := time.Now()
		b.observationMu.Lock()
		b.waitStarted = started
		b.observationMu.Unlock()
		defer func() {
			b.observationMu.Lock()
			b.waitDuration += time.Since(started)
			b.waitStarted = time.Time{}
			b.observationMu.Unlock()
		}()
		timer := time.NewTimer(required - elapsed)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	b.at = time.Now()
	b.bytes = n
	return ctx.Err()
}

func (b *ioBudget) observation() (bool, time.Duration) {
	b.observationMu.Lock()
	defer b.observationMu.Unlock()
	if !b.waitStarted.IsZero() {
		return true, b.waitDuration + time.Since(b.waitStarted)
	}
	return false, b.waitDuration
}
