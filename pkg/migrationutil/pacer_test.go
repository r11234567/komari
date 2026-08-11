package migrationutil

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPacerHalfDutyCycleMatchesWorkWithIdleTime(t *testing.T) {
	clock := time.Unix(1, 0)
	var waits []time.Duration
	pacer := NewPacer(0.5)
	pacer.now = func() time.Time { return clock }
	pacer.wait = func(_ context.Context, duration time.Duration) error {
		waits = append(waits, duration)
		clock = clock.Add(duration)
		return nil
	}

	if err := pacer.Pace(context.Background()); err != nil {
		t.Fatalf("start pacer: %v", err)
	}
	clock = clock.Add(100 * time.Millisecond)
	if err := pacer.Pace(context.Background()); err != nil {
		t.Fatalf("pace migration: %v", err)
	}
	if len(waits) != 1 || waits[0] != 100*time.Millisecond {
		t.Fatalf("waits = %v, want [100ms]", waits)
	}
}

func TestPacerPropagatesCancellation(t *testing.T) {
	clock := time.Unix(1, 0)
	pacer := NewPacer(0.5)
	pacer.now = func() time.Time { return clock }
	pacer.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	if err := pacer.Pace(context.Background()); err != nil {
		t.Fatalf("start pacer: %v", err)
	}
	clock = clock.Add(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pacer.Pace(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Pace() error = %v, want context canceled", err)
	}
}
