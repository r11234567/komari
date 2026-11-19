package metricstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

func TestStoreLeaseBlocksReloadAndClose(t *testing.T) {
	for _, operation := range []string{"reload", "close"} {
		t.Run(operation, func(t *testing.T) {
			s, err := metric.Open(context.Background(), metric.SQLite(":memory:"))
			if err != nil {
				t.Fatal(err)
			}
			installTestStore(t, s)
			active, release, err := AcquireStore(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if active != s {
				t.Fatal("lease did not pin the active instance")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			if operation == "reload" {
				err = Reload(ctx)
			} else {
				err = CloseStoreContext(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s must wait for the reader: %v", operation, err)
			}
			if _, err := active.ListMetrics(context.Background()); err != nil {
				t.Fatalf("waiting %s retired the leased store: %v", operation, err)
			}
			release()
			release() // Cleanup may also release on an early-return path.
			if err := CloseStoreContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := active.ListMetrics(context.Background()); !errors.Is(err, metric.ErrClosed) {
				t.Fatalf("released instance was not closed: %v", err)
			}
		})
	}
}

func TestStoreLeaseWaitCancellationAndMissingStore(t *testing.T) {
	s, err := metric.Open(context.Background(), metric.SQLite(":memory:"))
	if err != nil {
		t.Fatal(err)
	}
	installTestStore(t, s)
	if !storeOperations.TryAcquire() {
		t.Fatal("could not hold the exclusive gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	active, release, err := AcquireStore(ctx)
	storeOperations.Release()
	if !errors.Is(err, context.DeadlineExceeded) || active != nil || release != nil {
		t.Fatalf("canceled acquisition returned a lease: %v", err)
	}
	if !storeOperations.TryAcquire() {
		t.Fatal("canceled acquisition leaked a shared lease")
	}
	storeOperations.Release()
	if err := CloseStoreContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	active, release, err = AcquireStore(context.Background())
	if !errors.Is(err, ErrStoreNotInitialized) || active != nil || release != nil {
		t.Fatalf("missing store returned a lease: %v", err)
	}
	if !storeOperations.TryAcquire() {
		t.Fatal("missing-store acquisition leaked a shared lease")
	}
	storeOperations.Release()
}

func TestTrafficBaselineReusesLeaseWhileCloseWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := metric.Open(ctx, metric.SQLiteInDir(t.TempDir(), metric.WithSQLiteReadPool(2)))
	if err != nil {
		t.Fatal(err)
	}
	installTestStore(t, s)
	if err := EnsureBuiltinMetricDefinitions(ctx, s); err != nil {
		t.Fatal(err)
	}
	// Hold the read pool until the traffic query has acquired its lease. The
	// empty fixture forces its fallback through the latest-counter helper.
	stats, ok := s.PoolStats()["read"]
	if !ok || stats.MaxOpenConnections != 2 {
		t.Fatal("fixture requires an independent read pool")
	}
	var held []*sql.Rows
	closeRows := func() {
		for _, rows := range held {
			_ = rows.Close()
		}
		held = nil
	}
	defer closeRows()
	for range stats.MaxOpenConnections {
		rows, err := s.QueryContext(ctx, "SELECT 1 UNION ALL SELECT 2")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, rows)
		if !rows.Next() {
			t.Fatal("could not retain the SQL connection")
		}
	}
	waits := s.PoolStats()["read"].WaitCount
	readDone := make(chan error, 1)
	go func() {
		now := time.Now().UTC()
		_, _, err := GetTrafficRecordsByClientsAndTime(ctx, []string{"no-counters"}, now.Add(-time.Hour), now)
		readDone <- err
	}()
	for s.PoolStats()["read"].WaitCount <= waits {
		select {
		case err := <-readDone:
			t.Fatalf("traffic query did not wait for SQL: %v", err)
		case <-ctx.Done():
			t.Fatal("traffic query did not reach SQL")
		case <-time.After(time.Millisecond):
		}
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- CloseStoreContext(ctx) }()
	select {
	case err := <-closeDone:
		t.Fatalf("close did not wait for the query: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	closeRows()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("traffic fallback failed while close was queued: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("traffic fallback nested its lease behind a waiting close")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("close did not complete after traffic query released its lease")
	}
}
