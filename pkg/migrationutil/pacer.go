package migrationutil

import (
	"context"
	"sync"
	"time"
)

// Pacer limits a background migration by matching each work interval with an
// idle interval. A duty cycle of 0.5 keeps CPU-bound migration work near half
// of one core while leaving request handling and live writes time to run.
type Pacer struct {
	mu          sync.Mutex
	dutyCycle   float64
	initialized bool
	lastResume  time.Time
	now         func() time.Time
	wait        func(context.Context, time.Duration) error
}

func NewPacer(dutyCycle float64) *Pacer {
	if dutyCycle <= 0 || dutyCycle > 1 {
		dutyCycle = 0.5
	}
	return &Pacer{
		dutyCycle: dutyCycle,
		now:       time.Now,
		wait:      waitContext,
	}
}

// Pace yields after the work completed since the previous call. The first
// call starts measurement and returns immediately.
func (p *Pacer) Pace(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	if !p.initialized {
		p.initialized = true
		p.lastResume = now
		return nil
	}
	work := now.Sub(p.lastResume)
	if work <= 0 || p.dutyCycle >= 1 {
		p.lastResume = now
		return nil
	}
	wait := time.Duration(float64(work) * (1 - p.dutyCycle) / p.dutyCycle)
	if wait <= 0 {
		p.lastResume = now
		return nil
	}
	err := p.wait(ctx, wait)
	p.lastResume = p.now()
	return err
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
