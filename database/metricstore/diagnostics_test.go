package metricstore

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/komari-monitor/komari/protocol/v1"
)

func TestDiagnosticsReportQueueReflectsRealEnqueueAndOverflow(t *testing.T) {
	w := &reportBatchWorker{queue: make(chan v1.Report, 2)}
	reportBatcherMu.Lock()
	before := reportBatcher
	reportBatcher = w
	reportBatcherMu.Unlock()
	t.Cleanup(func() { reportBatcherMu.Lock(); reportBatcher = before; reportBatcherMu.Unlock() })
	for range 2 {
		if err := w.enqueue(context.Background(), v1.Report{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.enqueue(context.Background(), v1.Report{}); !errors.Is(err, ErrReportBatchQueueFull) {
		t.Fatalf("overflow changed: %v", err)
	}
	s := ReportQueueDiagnostics()
	if !s.Running || s.Depth != 2 || s.Capacity != 2 || s.Accepted != 2 || s.Rejected != 1 {
		t.Fatalf("queue diagnostics not from actual queue: %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.enqueue(ctx, v1.Report{}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled enqueue accepted")
	}
	<-w.queue
	if s := ReportQueueDiagnostics(); s.Depth != 1 || s.Rejected != 1 {
		t.Fatalf("snapshot did not follow drain: %+v", s)
	}
}
