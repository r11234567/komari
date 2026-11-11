package metricstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
	v1 "github.com/komari-monitor/komari/protocol/v1"
)

type reportIdentityKey struct{}
type reportIdentity struct{ id, proof string }

// WithReportIdentity is called only after authentication and protocol
// validation. The old Agent's process-local sequence is paired with its exact
// observed timestamp. A retry keeps both; a restarted Agent's new sample does
// not collide merely because its sequence starts at one again.
func WithReportIdentity(ctx context.Context, id, proof string) context.Context {
	return context.WithValue(ctx, reportIdentityKey{}, reportIdentity{id, proof})
}
func HasReportIdentity(ctx context.Context) bool {
	_, ok := ctx.Value(reportIdentityKey{}).(reportIdentity)
	return ok
}

func TelemetryEnabled() bool {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return store != nil && store.Telemetry() != nil
}

func TelemetryTrafficPending(ctx context.Context) (bool, error) {
	s, release, err := AcquireStore(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if s.Telemetry() == nil {
		return false, nil
	}
	return s.Telemetry().TrafficPending(ctx)
}

// DeliverTelemetryTraffic holds the same lifecycle lease as ingest and reload.
// Only background and explicit offline/test operations call this handoff.
// Foreground billing and dashboard reads never advance accounting.
func DeliverTelemetryTraffic(ctx context.Context, limit int, consume func(context.Context, metric.TelemetryTrafficEvent) error) (int, error) {
	s, release, err := AcquireStore(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	if s.Telemetry() == nil {
		return 0, nil
	}
	return s.Telemetry().DeliverTraffic(ctx, limit, consume)
}

func writeTelemetryReport(ctx context.Context, s *metric.Store, report v1.Report) (v1.Report, error) {
	if EntityWritesBlocked(report.UUID) {
		return v1.Report{}, ErrMetricWriteBlocked
	}
	id, ok := ctx.Value(reportIdentityKey{}).(reportIdentity)
	if !ok {
		// Legacy report transports have no stable sample identity. Preserve each
		// real submission rather than collapsing equal-valued measurements.
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return v1.Report{}, err
		}
		id.id = "legacy/" + hex.EncodeToString(b[:])
	}
	points := reportMetricPoints(report, 0, 0)
	// Traffic deltas are owned by the independent integer ledger, never by a
	// restart-time raw-history lookup or a floating-point metric counter.
	filtered := points[:0]
	defs, err := s.ListMetrics(ctx)
	if err != nil {
		return v1.Report{}, err
	}
	enabled := make(map[string]bool, len(defs))
	for _, d := range defs {
		enabled[d.Name] = d.RetentionDays > 0
	}
	for _, p := range points {
		if enabled[p.MetricName] {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) > 256 {
		return v1.Report{}, fmt.Errorf("report exceeds telemetry point budget")
	}
	_, err = s.Telemetry().Accept(ctx, metric.TelemetryBatch{Source: report.UUID, Identity: id.id, PayloadProof: id.proof, Points: filtered, Traffic: &metric.TelemetryTraffic{ObservedAt: report.UpdatedAt.UnixNano(), TotalUp: report.Network.TotalUp, TotalDown: report.Network.TotalDown, Up: report.Network.Up, Down: report.Network.Down}}, time.Now().UTC())
	if err != nil {
		return v1.Report{}, err
	}
	return report, nil
}
