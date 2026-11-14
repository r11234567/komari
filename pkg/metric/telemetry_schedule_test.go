package metric

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTelemetryScheduleFailurePersistsAndDoesNotStarve(t *testing.T) {
	dir := t.TempDir()
	s, e := telemetryFixture(t, dir)
	ctx := context.Background()
	now := time.Now()
	if err := e.EnsureSchedule(ctx, []ScheduleTask{{Name: "bad", Due: now}, {Name: "good", Due: now}}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("write deadline")
	var ended time.Time
	task, _, ran, err := e.DispatchSchedule(ctx, now, func(context.Context, ScheduleTask) (ScheduleOutcome, error) {
		ended = time.Now()
		return ScheduleOutcome{}, failure
	})
	if !errors.Is(err, failure) || !ran || task.Name != "bad" {
		t.Fatal(task, ran, err)
	}
	var due int64
	var attempts int
	if err = s.db.QueryRow("SELECT due,attempts FROM metric_telemetry_schedule WHERE task='bad'").Scan(&due, &attempts); err != nil {
		t.Fatal(err)
	}
	if d := time.Unix(0, due).Sub(ended); attempts != 1 || d < time.Minute || d > 73*time.Second {
		t.Fatal("failure backoff", attempts, d)
	}
	e.Close()
	s.Close()
	s, e = telemetryFixture(t, dir)
	if err = e.EnsureSchedule(ctx, []ScheduleTask{{Name: "bad", Due: now}}); err != nil {
		t.Fatal(err)
	}
	if err = e.ExpediteSchedule(ctx, []string{"bad"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var persisted int64
	if err = s.db.QueryRow("SELECT due FROM metric_telemetry_schedule WHERE task='bad'").Scan(&persisted); err != nil || persisted != due {
		t.Fatal("restart/input erased backoff", persisted, due, err)
	}
	task, _, ran, err = e.DispatchSchedule(ctx, time.Now(), func(context.Context, ScheduleTask) (ScheduleOutcome, error) {
		return ScheduleOutcome{Delay: time.Hour, Cursor: "last-metric"}, nil
	})
	if err != nil || !ran || task.Name != "good" {
		t.Fatal("failed task starved other work", task, ran, err)
	}
	_, _, ran, err = e.DispatchSchedule(ctx, time.Now(), func(context.Context, ScheduleTask) (ScheduleOutcome, error) {
		t.Fatal("ran before due")
		return ScheduleOutcome{}, nil
	})
	if ran || err != nil {
		t.Fatal(ran, err)
	}
	for i, base := range []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if d := telemetryRetryDelay(i + 1); d < base || d > base+base/5 {
			t.Fatal(i, d)
		}
	}
}

func TestTelemetryScheduleExcludesOtherStoreAndRecoversLease(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	other, err := s.OpenTelemetry(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	now := time.Now()
	if err = e.EnsureSchedule(ctx, []ScheduleTask{{Name: "one", Due: now}, {Name: "two", Due: now}}); err != nil {
		t.Fatal(err)
	}
	_, _, ran, err := e.DispatchSchedule(ctx, now, func(context.Context, ScheduleTask) (ScheduleOutcome, error) {
		_, _, did, blocked := other.DispatchSchedule(ctx, time.Now(), func(context.Context, ScheduleTask) (ScheduleOutcome, error) {
			t.Error("second worker ran")
			return ScheduleOutcome{}, nil
		})
		if did || !errors.Is(blocked, ErrTelemetryBackpressure) {
			t.Error("worker exclusion", did, blocked)
		}
		return ScheduleOutcome{Delay: time.Hour}, nil
	})
	if err != nil || !ran {
		t.Fatal(ran, err)
	}
	// A process died after claiming 'two'; no data progress is rewound. Only
	// its expired control lease is reclaimed by a different Store owner.
	if _, err = s.db.Exec("UPDATE metric_telemetry_schedule SET owner='dead',expires=? WHERE task='two'", time.Now().Add(-time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	task, _, ran, err := other.DispatchSchedule(ctx, time.Now(), func(context.Context, ScheduleTask) (ScheduleOutcome, error) {
		return ScheduleOutcome{Delay: time.Hour}, nil
	})
	if err != nil || !ran || task.Name != "two" {
		t.Fatal(task, ran, err)
	}
}

func TestTelemetryScheduleCancelledWorkRecordsBackoff(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.EnsureSchedule(ctx, []ScheduleTask{{Name: "cancel", Due: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	_, _, ran, err := e.DispatchSchedule(ctx, time.Now(), func(ctx context.Context, _ ScheduleTask) (ScheduleOutcome, error) {
		cancel()
		return ScheduleOutcome{}, ctx.Err()
	})
	if !ran || !errors.Is(err, context.Canceled) {
		t.Fatal(ran, err)
	}
	var attempts int
	var owner string
	if err = s.db.QueryRow("SELECT attempts,owner FROM metric_telemetry_schedule WHERE task='cancel'").Scan(&attempts, &owner); err != nil || attempts != 1 || owner != "" {
		t.Fatal(attempts, owner, err)
	}
}
