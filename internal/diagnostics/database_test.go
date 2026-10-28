package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

func TestDiagnosticsRealSQLitePoolWaitAndQueryCancellation(t *testing.T) {
	var counters QueryCounters
	db, err := gorm.Open(sqlite.Open("file:"+t.TempDir()+"/synthetic.db?_busy_timeout=1000"), &gorm.Config{Logger: QueryLogger{Interface: gormlog.Discard, Counters: &counters}})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	if err = db.Exec("CREATE TABLE synthetic (value TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("INSERT INTO synthetic VALUES (?)", "synthetic-private-SQL-value").Error; err != nil {
		t.Fatal(err)
	}
	held, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before := PoolStats(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = db.WithContext(ctx).Exec("INSERT INTO synthetic VALUES (?)", "must-not-be-written").Error
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query did not wait/cancel against the real single-connection pool: %v", err)
	}
	after := PoolStats(pool)
	if after.InUse != 1 || after.WaitCount <= before.WaitCount || after.WaitDurationNS <= before.WaitDurationNS {
		t.Fatalf("pool contention not measured: before=%+v after=%+v", before, after)
	}
	q := counters.Snapshot()
	if q.Count != 3 || q.Errors != 1 || q.Canceled != 1 || q.DurationNS <= 0 {
		t.Fatalf("query counters: %+v", q)
	}
	raw, _ := json.Marshal(q)
	if strings.Contains(string(raw), "synthetic-private") {
		t.Fatal("query value leaked into metrics")
	}
	held.Close()
	var count int64
	if err = db.Table("synthetic").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("canceled insert executed: count=%d error=%v", count, err)
	}
}

func TestDiagnosticsQueryLoggerDoesNotRenderOrChangeLogMode(t *testing.T) {
	var counters QueryCounters
	l := QueryLogger{Interface: gormlog.Discard, Counters: &counters}
	fc := func() (string, int64) {
		t.Fatal("instrumentation rendered SQL although wrapped logger is silent")
		return "", 0
	}
	l.LogMode(gormlog.Silent).Trace(context.Background(), time.Now(), fc, gorm.ErrRecordNotFound)
	q := counters.Snapshot()
	if q.Count != 1 || q.NotFound != 1 || q.Errors != 0 {
		t.Fatalf("record-not-found classification: %+v", q)
	}
}
