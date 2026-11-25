package metric

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

type TrafficCounters struct {
	Sent     int64 `json:"sent"`
	Received int64 `json:"received"`
}

type TrafficSample struct {
	Stream     string                     `json:"stream"`
	Sequence   int64                      `json:"sequence"`
	Observed   int64                      `json:"observed"`
	BootID     string                     `json:"boot_id"`
	Booted     int64                      `json:"booted"`
	Interfaces map[string]TrafficCounters `json:"interfaces"`
	GapStart   int64                      `json:"gap_start,omitempty"`
	Cutover    int64                      `json:"cutover,omitempty"`
}

func (t *Telemetry) TrafficSampleWindow() time.Duration { return t.cfg.MaxLateness }

func (t *Telemetry) LatestTrafficSample(ctx context.Context, source string) (time.Time, error) {
	var observed int64
	err := t.s.reader().QueryRowContext(ctx, t.bind("SELECT observed FROM "+t.table("traffic_stream")+" WHERE source=?"), source).Scan(&observed)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, observed).UTC(), nil
}

func (t *Telemetry) prepareTraffic(ctx context.Context, tx *sql.Tx, b *TelemetryBatch, observed int64) (bool, error) {
	var prior TrafficSample
	var payload string
	err := tx.QueryRowContext(ctx, t.bind("SELECT payload FROM "+t.table("traffic_stream")+" WHERE source=?"), b.Source).Scan(&payload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(payload), &prior); err != nil {
			return false, err
		}
	}
	if b.TrafficSample == nil {
		if prior.Stream == "" {
			return false, t.deriveTraffic(ctx, tx, b, observed)
		}
		// Once a dedicated stream has committed, monitor reports cannot move
		// the billing baseline or emit competing traffic history points.
		b.Traffic = nil
		points := b.Points[:0]
		for _, point := range b.Points {
			if point.MetricName != "traffic.up" && point.MetricName != "traffic.down" {
				points = append(points, point)
			}
		}
		b.Points = points
		return false, nil
	}
	sample := *b.TrafficSample
	if sample.Stream == "" || len(sample.Stream) > 64 || sample.Sequence < 1 || sample.Observed < 1 || len(sample.Interfaces) > 64 || sample.GapStart > sample.Observed {
		return false, fmt.Errorf("%w: traffic sample", ErrInvalidArgument)
	}
	sameStream := sample.Stream == prior.Stream
	if sameStream && sample.Sequence <= prior.Sequence {
		return true, nil
	}
	if prior.Observed != 0 && sample.Observed <= prior.Observed {
		return false, fmt.Errorf("%w: traffic observation did not advance", ErrTelemetryConflict)
	}
	// An initial dedicated sample never bridges into the Agent's old monthly
	// totals. Those totals need not have the same counter origin.
	sample.Cutover = prior.Cutover
	if prior.Observed == 0 {
		err = tx.QueryRowContext(ctx, t.bind("SELECT observed FROM "+t.table("traffic_ingress")+" WHERE source=?"), b.Source).Scan(&sample.Cutover)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	traffic, err := measuredTraffic(prior, sample, sameStream)
	if err != nil {
		return false, err
	}
	if prior.Observed < sample.Cutover {
		traffic.DeltaUp, traffic.DeltaDown = 0, 0
		if sample.Observed > sample.Cutover && traffic.GapStart == 0 {
			traffic.GapStart = sample.Cutover
		}
	}
	if sample.Sequence > 1 && (!sameStream || sample.Sequence != prior.Sequence+1) && traffic.GapStart == 0 {
		traffic.GapStart = max(prior.Observed, sample.Cutover)
		if traffic.GapStart == 0 {
			traffic.GapStart = sample.Observed
		}
	}
	b.Traffic = &traffic
	for i := range b.Points {
		switch b.Points[i].MetricName {
		case "traffic.up":
			b.Points[i].Value = float64(traffic.DeltaUp)
		case "traffic.down":
			b.Points[i].Value = float64(traffic.DeltaDown)
		}
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		return false, err
	}
	query := "INSERT INTO " + t.table("traffic_stream") + " (source,observed,stream,sequence,payload) VALUES (?,?,?,?,?)"
	if t.s.cfg.Driver == DriverMySQL {
		query += " ON DUPLICATE KEY UPDATE observed=VALUES(observed),stream=VALUES(stream),sequence=VALUES(sequence),payload=VALUES(payload)"
	} else {
		query += " ON CONFLICT(source) DO UPDATE SET observed=excluded.observed,stream=excluded.stream,sequence=excluded.sequence,payload=excluded.payload"
	}
	_, err = tx.ExecContext(ctx, t.bind(query), b.Source, sample.Observed, sample.Stream, sample.Sequence, string(encoded))
	return false, err
}

func measuredTraffic(prior, sample TrafficSample, sameStream bool) (TelemetryTraffic, error) {
	traffic := TelemetryTraffic{Dedicated: true, HasDelta: true, ObservedAt: sample.Observed, GapStart: sample.GapStart}
	if sameStream && prior.Observed != 0 {
		traffic.Start = prior.Observed
	}
	reboot := sameStream && prior.BootID != "" && sample.BootID != "" && prior.BootID != sample.BootID && sample.Booted > prior.Observed && sample.Booted <= sample.Observed
	continuous := sameStream && prior.BootID == sample.BootID && (sample.BootID != "" || prior.Booted == sample.Booted)
	if reboot {
		traffic.Start = sample.Booted
		traffic.GapStart = prior.Observed
	} else if prior.Observed != 0 && !continuous {
		traffic.GapStart = prior.Observed
	}
	for id, now := range sample.Interfaces {
		if id == "" || len(id) > 128 || now.Sent < 0 || now.Received < 0 {
			return traffic, fmt.Errorf("%w: invalid interface counter", ErrInvalidArgument)
		}
		if !addTrafficCounters(&traffic.TotalUp, &traffic.TotalDown, now) {
			return traffic, fmt.Errorf("%w: interface counter overflow", ErrInvalidArgument)
		}
		var delta TrafficCounters
		if reboot {
			if _, monitored := prior.Interfaces[id]; monitored {
				delta = now
			}
		} else if previous, found := prior.Interfaces[id]; continuous && found {
			delta = TrafficCounters{Sent: now.Sent - previous.Sent, Received: now.Received - previous.Received}
		}
		if traffic.Start > 0 {
			// Even a 1 Tb/s link can only transfer this many bytes in the
			// measured interval. Rebase a counter independently per direction.
			elapsed := time.Duration(sample.Observed - traffic.Start)
			limit := int64(min(elapsed, 48*time.Hour).Seconds()*125_000_000_000) + 125_000_000_000
			if delta.Sent < 0 || delta.Sent > limit {
				delta.Sent = 0
				traffic.GapStart = prior.Observed
			}
			if delta.Received < 0 || delta.Received > limit {
				delta.Received = 0
				traffic.GapStart = prior.Observed
			}
		}
		if !addTrafficCounters(&traffic.DeltaUp, &traffic.DeltaDown, delta) {
			return traffic, fmt.Errorf("%w: interface delta overflow", ErrInvalidArgument)
		}
	}
	return traffic, nil
}

func addTrafficCounters(up, down *int64, counters TrafficCounters) bool {
	if counters.Sent > math.MaxInt64-*up || counters.Received > math.MaxInt64-*down {
		return false
	}
	*up += counters.Sent
	*down += counters.Received
	return true
}
