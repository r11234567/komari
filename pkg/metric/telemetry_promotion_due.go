package metric

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// NextSummaryPromotion seeks the oldest unhanded input of each physical tier
// through summary_pending. It reads keys, never historical stats or digests.
// The durable scheduler stores the resulting deadline and does no work early.
func (t *Telemetry) NextSummaryPromotion(ctx context.Context, now time.Time) (time.Duration, error) {
	if t.scalar {
		return t.scalarPromotionDelay(ctx, now)
	}
	through, err := t.progress(ctx, t.s.reader(), "summary")
	if err != nil {
		return 0, err
	}
	delay := 24 * time.Hour
	for _, tier := range telemetryPromotionTiers() {
		var bucket int64
		q := "SELECT b.bucket FROM " + t.table("summary") + " b JOIN " + t.table("segments") + " a ON a.id=b.segment_id WHERE b.resolution=? AND b.promoted_through=0 AND (a.summary_version=2 OR (a.summary_version=1 AND b.resolution>=?)) AND a.state NOT IN ('open','legacy') AND a.last_id<=? ORDER BY b.bucket LIMIT 1"
		err = t.s.reader().QueryRowContext(ctx, t.bind(q), int64(tier.source), int64(15*time.Minute), through).Scan(&bucket)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		end := alignTime(time.Unix(0, bucket).UTC(), tier.target).Add(tier.target)
		due := end.Add(tier.keep - telemetryPromotionLead(tier.keep, tier.target))
		delay = min(delay, max(time.Second, due.Sub(now)))
	}
	return delay, nil
}
