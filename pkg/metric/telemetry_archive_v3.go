package metric

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

// A v3 extent is ordinary point JSONL inside a checksummed zstd frame. Its
// bounded footer provides selection and count; there is no nested SHA footer.
func (t *Telemetry) visitScalarArchiveFrame(ctx context.Context, seg telemetrySegment, input io.Reader, visit func(telemetryRecord) error) error {
	decoder, err := zstd.NewReader(input, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(2<<20))
	if err != nil {
		return err
	}
	defer decoder.Close()
	scanner := bufio.NewScanner(io.LimitReader(decoder, telemetryFramePlainLimit+1))
	scanner.Buffer(make([]byte, 8192), 64<<10)
	var count, last int64
	ordinal := -1
	plain := 0
	for scanner.Scan() {
		if err = ctx.Err(); err != nil {
			return err
		}
		plain += len(scanner.Bytes()) + 1
		if plain > telemetryFramePlainLimit {
			return errors.New("archive frame exceeds plain byte limit")
		}
		var record telemetryRecord
		if err = json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return err
		}
		if record.Kind != "point" || record.Point == nil || record.Ordinal < 0 || record.Ingest < seg.First || record.Ingest > seg.Last || record.Ingest < last || (record.Ingest == last && record.Ordinal <= ordinal) {
			return errors.New("invalid archive point order")
		}
		if err = record.Point.Validate(); err != nil {
			return err
		}
		if len(record.Point.Tags) > 32 || len(record.Point.Labels) > 32 {
			return errors.New("archive label budget exceeded")
		}
		last, ordinal = record.Ingest, record.Ordinal
		count++
		if visit != nil {
			if err = visit(record); err != nil {
				return err
			}
		}
		if count%128 == 0 {
			if err = telemetryYield(ctx); err != nil {
				return err
			}
		}
	}
	if err = scanner.Err(); err != nil {
		return err
	}
	if count != seg.Points {
		return errors.New("archive frame point count mismatch")
	}
	return nil
}

func telemetryFrameMatches(ctx context.Context, frame telemetryArchiveFrame, queries []Query) bool {
	if frame.Version != 3 {
		return true
	} // bounded compatibility scan for old files
	if allowed, ok := ctx.Value(telemetryScopeKey{}).([]string); ok {
		found := false
		for _, entity := range allowed {
			if frame.Entity == entity {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, q := range queries {
		if q.EntityID != "" && q.EntityID != frame.Entity {
			continue
		}
		if frame.ObservedLast >= q.Start.UnixNano() && frame.ObservedFirst <= q.End.UnixNano() {
			return true
		}
	}
	return false
}
