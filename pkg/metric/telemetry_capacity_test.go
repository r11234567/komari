package metric

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTelemetryDiskReservePreservesACKAndProtectsIngress(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	b := telemetryBatch("accepted", now, 1)
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	e.capacity.Lock()
	e.capacity.checked = time.Now()
	e.capacity.free = 32 << 20
	e.capacity.err = nil
	e.capacity.Unlock()
	if r, err := e.Accept(ctx, b, now); err != nil || !r.Duplicate {
		t.Fatal("durable duplicate ACK lost under pressure", r, err)
	}
	if _, err := e.Accept(ctx, telemetryBatch("new", now.Add(time.Second), 2), now.Add(time.Second)); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatal(err)
	}
	if n := telemetryCount(t, e, "receipts"); n != 1 {
		t.Fatal("pressure accepted new input", n)
	}
	e.capacity.Lock()
	e.capacity.free = 128 << 20
	e.capacity.Unlock()
	if err := e.CheckCapacity(ctx, false); err != nil {
		t.Fatal("minimum ingress reserve unavailable", err)
	}
	if err := e.CheckCapacity(ctx, true); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatal("low priority work consumed reserved disk", err)
	}
}
