package diagnostics

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

type QueryCounters struct {
	count, errors, canceled, notFound, durationNS, maxNS atomic.Int64
}

var PrimaryQueries QueryCounters

type QuerySnapshot struct {
	Count      int64 `json:"count"`
	Errors     int64 `json:"errors"`
	Canceled   int64 `json:"canceled"`
	NotFound   int64 `json:"not_found"`
	DurationNS int64 `json:"duration_sum_ns"`
	MaxNS      int64 `json:"duration_max_ns"`
}

func (q *QueryCounters) Observe(duration time.Duration, err error) {
	q.count.Add(1)
	q.durationNS.Add(int64(duration))
	raise(&q.maxNS, int64(duration))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		q.notFound.Add(1)
	} else if err != nil {
		q.errors.Add(1)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		q.canceled.Add(1)
	}
}
func (q *QueryCounters) Snapshot() QuerySnapshot {
	return QuerySnapshot{Count: q.count.Load(), Errors: q.errors.Load(), Canceled: q.canceled.Load(), NotFound: q.notFound.Load(), DurationNS: q.durationNS.Load(), MaxNS: q.maxNS.Load()}
}

// QueryLogger preserves the existing logger and never invokes its SQL-rendering
// callback itself. Successful instrumentation needs no query text or labels.
type QueryLogger struct {
	gormlog.Interface
	Counters *QueryCounters
}

func (l QueryLogger) LogMode(level gormlog.LogLevel) gormlog.Interface {
	l.Interface = l.Interface.LogMode(level)
	return l
}
func (l QueryLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	l.Counters.Observe(time.Since(begin), err)
	l.Interface.Trace(ctx, begin, fc, err)
}

type PoolSnapshot struct {
	MaxOpen        int   `json:"max_open"`
	Open           int   `json:"open"`
	InUse          int   `json:"in_use"`
	Idle           int   `json:"idle"`
	WaitCount      int64 `json:"wait_count"`
	WaitDurationNS int64 `json:"wait_duration_ns"`
}

func PoolStats(db *sql.DB) PoolSnapshot {
	s := db.Stats()
	return PoolSnapshot{MaxOpen: s.MaxOpenConnections, Open: s.OpenConnections, InUse: s.InUse, Idle: s.Idle, WaitCount: s.WaitCount, WaitDurationNS: int64(s.WaitDuration)}
}
