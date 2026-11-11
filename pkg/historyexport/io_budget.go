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
	mu          sync.Mutex
	rate        int64
	at, checked time.Time
	bytes       uint64
}

func (b *ioBudget) wait(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.checked) < 100*time.Millisecond {
		return ctx.Err()
	}
	now := time.Now()
	n, e := processIOBytes()
	if e != nil {
		return e
	}
	b.checked = now
	if b.at.IsZero() {
		b.at = now
		b.bytes = n
		return ctx.Err()
	}
	elapsed := now.Sub(b.at)
	required := time.Duration(float64(n-b.bytes) / float64(b.rate) * float64(time.Second))
	if required > elapsed {
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
