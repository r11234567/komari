package metric

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

func releaseChartMemory(ctx context.Context, n int) {
	if b, ok := ctx.Value(telemetryChartBudgetKey{}).(*telemetryChartBudget); ok {
		b.bytes += n
	}
}

func defaultChartBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(telemetryChartBudgetKey{}).(*telemetryChartBudget); !ok {
		return context.WithValue(ctx, telemetryChartBudgetKey{}, &telemetryChartBudget{points: 100000, bytes: 32 << 20})
	}
	return ctx
}

// Reserve JSON/base64 scratch before inspecting the envelope, then reserve its
// declared decoded size before zstd allocates. Only the retained digest remains
// charged after decoding. A compressed SQL length is never a memory bound.
func decodeChartStats(ctx context.Context, raw string, out *telemetryStats) error {
	if strings.HasPrefix(strings.TrimSpace(raw), `"`) && len(raw) > base64.StdEncoding.EncodedLen(telemetryStatsLimit)+16 {
		return errors.New("telemetry statistics envelope too large")
	}
	scratch := 3*len(raw) + 1024
	if err := reserveChartMemory(ctx, 0, scratch); err != nil {
		return err
	}
	defer func() { releaseChartMemory(ctx, scratch) }()
	decoded := len(raw)
	if strings.HasPrefix(strings.TrimSpace(raw), `"`) {
		var text string
		if err := json.Unmarshal([]byte(raw), &text); err != nil {
			return err
		}
		if !strings.HasPrefix(text, telemetryStatsPrefix) {
			return errors.New("unsupported telemetry statistics encoding")
		}
		payload := text[len(telemetryStatsPrefix):]
		var prefix [48]byte
		n, err := base64.StdEncoding.Decode(prefix[:], []byte(payload[:min(len(payload), 64)]))
		if err != nil {
			return err
		}
		var header zstd.Header
		if err = header.Decode(prefix[:n]); err != nil {
			return err
		}
		if !header.HasFCS || header.Skippable || header.FrameContentSize > telemetryStatsLimit {
			return errors.New("invalid telemetry statistics frame size")
		}
		decoded = int(header.FrameContentSize)
	}
	if err := reserveChartMemory(ctx, 0, 2*decoded); err != nil {
		return err
	}
	scratch += 2 * decoded
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return err
	}
	if scalar, _ := ctx.Value(telemetryScalarStatsKey{}).(bool); scalar {
		// Mean/count/min/max/variance charts never consume a distribution.
		// Retaining every input/output digest turns a small scalar result into
		// source-sized memory. Decoding scratch is still reserved beforehand.
		out.Digest, out.sketch = nil, nil
	} else {
		scratch -= cap(out.Digest)
	}
	return nil
}

func chartDigestBytes(raw []byte) (int, error) {
	if len(raw) < 3 || raw[0] != tdigestMagic0 || raw[1] != tdigestCompressedMagic1 {
		return len(raw), nil
	}
	if raw[2] != tdigestVersion {
		return 0, errors.New("metric: unsupported compressed t-digest version")
	}
	reader := flate.NewReader(bytes.NewReader(raw[3:]))
	defer reader.Close()
	var header [39]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, err
	}
	if header[0] != tdigestMagic0 || header[1] != tdigestMagic1 || header[2] != tdigestVersion {
		return 0, errors.New("metric: invalid compressed t-digest header")
	}
	n := uint64(binary.LittleEndian.Uint32(header[35:]))
	if n > ((64<<20)-39)/16 {
		return 0, errors.New("metric: compressed t-digest blob is too large")
	}
	return 39 + int(n)*16, nil
}

func chartStatsBytes(s *telemetryStats) int {
	n := cap(s.Digest)
	if s.sketch != nil {
		n += 128 + cap(s.sketch.centroids)*16
	}
	return n
}

// DecodeTDigest and Merge allocate input centroids, append capacity and a fresh
// sorted result. Reserve their peak before merging, then retain the actual
// result allocation. Transient work is refunded so large valid queries do not
// accumulate a charge for scratch that has already been released.
func mergeChartStats(ctx context.Context, dst *telemetryStats, src telemetryStats) error {
	scratch := 1024
	for _, s := range []*telemetryStats{dst, &src} {
		if len(s.Digest) >= 3 && s.Digest[1] == tdigestCompressedMagic1 {
			scratch += 64 << 10
		}
	}
	if err := reserveChartMemory(ctx, 0, scratch); err != nil {
		return err
	}
	before := chartStatsBytes(dst)
	defer func() { releaseChartMemory(ctx, scratch) }()
	a, err := chartDigestBytes(dst.Digest)
	if err != nil {
		return err
	}
	b, err := chartDigestBytes(src.Digest)
	if err != nil {
		return err
	}
	working := 6 * (a + b + before + chartStatsBytes(&src))
	if err = reserveChartMemory(ctx, 0, working); err != nil {
		return err
	}
	scratch += working
	if err = dst.merge(src); err != nil {
		return err
	}
	if card, _ := ctx.Value(telemetryStatisticsCardKey{}).(bool); card && dst.N > 0 {
		if dst.sketch == nil {
			dst.sketch, err = DecodeTDigest(dst.Digest)
			if err != nil {
				return err
			}
		}
		if dst.sketch != nil {
			// Match the final card's existing higher-precision merge, including
			// the first source cell, within the already reserved merge scratch.
			dst.sketch.compression = max(dst.sketch.compression, 400)
		}
	}
	// A recompressed sketch can shrink as well as grow. Its previous retained
	// allocation was already charged; refund the signed change so repeated
	// merges do not accumulate a budget for centroids that no longer exist.
	scratch -= chartStatsBytes(dst) - before
	return nil
}

func decodeChartDigest(ctx context.Context, raw []byte) (*TDigest, error) {
	scratch := 64 << 10
	if err := reserveChartMemory(ctx, 0, scratch); err != nil {
		return nil, err
	}
	defer func() { releaseChartMemory(ctx, scratch) }()
	n, err := chartDigestBytes(raw)
	if err != nil {
		return nil, err
	}
	if err = reserveChartMemory(ctx, 0, 2*n+128); err != nil {
		return nil, err
	}
	scratch += 2*n + 128
	d, err := DecodeTDigest(raw)
	if err != nil {
		return nil, err
	}
	scratch -= 128 + cap(d.centroids)*16
	return d, nil
}

func addChartStats(ctx context.Context, dst *telemetryStats, p Point, ingest int64, ordinal int) error {
	if dst.sketch == nil && len(dst.Digest) > 0 {
		d, err := decodeChartDigest(ctx, dst.Digest)
		if err != nil {
			return err
		}
		dst.sketch = d
	}
	before := chartStatsBytes(dst)
	scratch := 6 * (before + 1024)
	if err := reserveChartMemory(ctx, 0, scratch); err != nil {
		return err
	}
	dst.add(p, ingest, ordinal)
	if scalar, _ := ctx.Value(telemetryScalarStatsKey{}).(bool); scalar {
		dst.Digest, dst.sketch = nil, nil
	}
	releaseChartMemory(ctx, scratch-(chartStatsBytes(dst)-before))
	return nil
}
