package metric

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestTelemetryTrafficDeliverySerializesFreshnessReaders(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC()
	if err := e.SeedTrafficBaseline(ctx, "node", at.Add(-time.Second), 100, 200); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		b := telemetryBatch(string(rune('a'+i)), at.Add(time.Duration(i)*time.Second), 1)
		b.Traffic = &TelemetryTraffic{ObservedAt: b.Points[0].Timestamp.UnixNano(), TotalUp: int64(105 + i*5), TotalDown: int64(210 + i*10)}
		if _, err := e.Accept(ctx, b, at); err != nil {
			t.Fatal(err)
		}
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 2)
	var order []int64
	go func() {
		_, err := e.DeliverTraffic(ctx, 1, func(_ context.Context, event TelemetryTrafficEvent) error {
			close(entered)
			<-release
			order = append(order, event.IngestID)
			if event.Traffic.DeltaUp != 5 {
				t.Errorf("migration baseline lost %+v", event)
			}
			return nil
		})
		done <- err
	}()
	<-entered
	go func() {
		_, err := e.DeliverTraffic(ctx, 256, func(_ context.Context, event TelemetryTrafficEvent) error {
			order = append(order, event.IngestID)
			if !event.Observed.Equal(at.Add(time.Second)) {
				t.Errorf("observed time rounded %+v", event)
			}
			return nil
		})
		done <- err
	}()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(order, []int64{1, 2}) {
		t.Fatal("outbox reordered", order)
	}
}

func TestTelemetryTrafficDeltaAtomicRetryArchive(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-time.Hour)
	batch := func(id string, ts time.Time, total int64) TelemetryBatch {
		return TelemetryBatch{Source: "node", Identity: id, Points: []Point{{MetricName: "traffic.up", EntityID: "node", Timestamp: ts}}, Traffic: &TelemetryTraffic{ObservedAt: ts.UnixNano(), TotalUp: total, Up: 100}}
	}
	a := batch("baseline", at, 1<<54)
	if _, err := e.Accept(ctx, a, now); err != nil {
		t.Fatal(err)
	}
	b := batch("delta", at.Add(time.Second), (1<<54)+7)
	e.cfg.Fault = func(stage string) error {
		if stage == "ingest-before-commit" {
			return errors.New("rollback")
		}
		return nil
	}
	if _, err := e.Accept(ctx, b, now); err == nil {
		t.Fatal("injected transaction succeeded")
	}
	e.cfg.Fault = nil
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	if r, err := e.Accept(ctx, b, now); err != nil || !r.Duplicate {
		t.Fatal(r, err)
	}
	c := batch("next", at.Add(2*time.Second), (1<<54)+10)
	if _, err := e.Accept(ctx, c, now); err != nil {
		t.Fatal(err)
	}
	q := Query{MetricName: "traffic.up", EntityID: "node", Start: at, End: now, Order: OrderAsc}
	check := func() {
		t.Helper()
		points, err := e.ExportRawBatch(ctx, []Query{q})
		if err != nil || len(points[0]) != 3 {
			t.Fatal(points, err)
		}
		for i, want := range []float64{0, 7, 3} {
			if points[0][i].Value != want {
				t.Fatalf("delta %d = %v, want %v", i, points[0][i].Value, want)
			}
		}
	}
	check()
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ArchiveNext(ctx); err != nil {
		t.Fatal(err)
	}
	var deltas []int64
	if n, err := e.DeliverTraffic(ctx, 32, func(_ context.Context, event TelemetryTrafficEvent) error {
		if !event.Traffic.HasDelta {
			t.Fatal("missing durable delta")
		}
		deltas = append(deltas, event.Traffic.DeltaUp)
		return nil
	}); err != nil || n != 3 {
		t.Fatal(n, err)
	}
	for i, want := range []int64{0, 7, 3} {
		if deltas[i] != want {
			t.Fatal(deltas)
		}
	}
	if _, err := e.DeleteArchivedChunk(ctx, now.Add(2*time.Hour), 512); err != nil {
		t.Fatal(err)
	}
	check()
}
