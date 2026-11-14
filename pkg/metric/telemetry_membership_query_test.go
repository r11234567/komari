package metric

import (
	"context"
	"testing"
	"time"
)

// Late reports and a retired hot table must remain queryable when selecting
// summary owners by observed-time membership rather than segment creation.
func TestTelemetryPlannerMembershipAfterLateArchiveRetirement(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-6 * time.Hour)
	if _, err := e.Accept(ctx, telemetryBatch("late-observation", at, 2, 5, 11), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for {
		more, err := e.ArchiveNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	for {
		n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if telemetryCount(t, e, "hot") != 0 {
		t.Fatal("fixture still has exact hot data")
	}
	if err := e.root.Close(); err != nil {
		t.Fatal(err)
	}
	// The segment was created six hours after the observations. Reading the
	// observations, not a.created, is required, including exact boundary hours.
	for _, interval := range []time.Duration{10 * time.Second, time.Minute, time.Hour} {
		q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: interval, Aggregation: AggSum, PreserveSeries: true}
		r, err := e.Chart(ctx, q, now)
		if err != nil || len(r.Points) != 1 || r.Points[0].Value != 18 || r.Points[0].Count != 3 || len(r.Gaps) != 0 {
			t.Fatalf("retired late input at %s: %+v, %v", interval, r, err)
		}
	}
}
