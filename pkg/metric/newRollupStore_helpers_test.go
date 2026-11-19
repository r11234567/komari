package metric

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func newRollupStore(t *testing.T, policy RollupPolicy) *Store {
	t.Helper()
	dsn := fmt.Sprintf("file:rollup-%d?mode=memory&cache=shared", time.Now().UnixNano())
	store, err := Open(context.Background(), SQLite(dsn, WithRollupPolicy(policy)))
	if err != nil {
		t.Fatalf("open rollup store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
