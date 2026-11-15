package metric

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// WritePoints is the adapter for Ping/custom measurements. Agent reports use
// Accept directly with their authenticated wire identity and integer counters.
func (t *Telemetry) WritePoints(ctx context.Context, points []Point) error {
	if len(points) > 256 {
		return ErrTelemetryBackpressure
	}
	points, err := t.s.filterDisabledMetricPoints(ctx, points)
	if err != nil || len(points) == 0 {
		return err
	}
	byEntity := make(map[string][]Point)
	for _, p := range points {
		if p.MetricName == sqliteVirtualPingLossMetric {
			continue
		}
		byEntity[p.EntityID] = append(byEntity[p.EntityID], p.normalized())
	}
	entities := make([]string, 0, len(byEntity))
	for entity := range byEntity {
		entities = append(entities, entity)
	}
	sort.Strings(entities)
	for _, entity := range entities {
		payload, err := json.Marshal(byEntity[entity])
		if err != nil {
			return err
		}
		hash := sha256.Sum256(payload)
		_, err = t.Accept(ctx, TelemetryBatch{Source: entity, Identity: "points/" + hex.EncodeToString(hash[:]), Points: byEntity[entity]}, time.Now().UTC())
		if err != nil {
			return err
		}
	}
	return nil
}
