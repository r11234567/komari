package metric

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Only immutable cells are compressed. Hot deltas keep their cheap JSON write
// path. The envelope is versioned, bounded and lossless, including every digest
// centroid and all ordering identities; no percentile is discarded or rounded.
const telemetryStatsLimit = 1 << 20
const telemetryStatsPrefix = "TSC1:"

var statsEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(64<<10), zstd.WithSingleSegment(true))
})
var statsDecoder = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(telemetryStatsLimit), zstd.WithDecoderMaxWindow(telemetryStatsLimit), zstd.WithDecodeAllCapLimit(true))
})
var statsEncodeMu, statsDecodeMu sync.Mutex

func (s telemetryStats) encodeSealed() (string, error) {
	if s.sketch != nil {
		s.Digest = s.sketch.encodeRaw()
		s.sketch = nil
	}
	plain, err := s.encode()
	if err != nil || len(s.Digest) > telemetryStatsLimit-256 {
		return plain, err
	}
	b := make([]byte, 0, 160+len(s.Digest))
	for _, n := range []int64{s.N, s.Attempts, s.Loss, s.FirstNano, s.LastNano, s.FirstIngest, s.LastIngest, int64(s.FirstOrdinal), int64(s.LastOrdinal)} {
		b = binary.AppendVarint(b, n)
	}
	for _, f := range []float64{s.Sum, s.Mean, s.M2, s.Min, s.Max, s.First, s.Last} {
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(f))
	}
	b = append(b, s.Digest...)
	e, err := statsEncoder()
	if err != nil {
		return "", err
	}
	statsEncodeMu.Lock()
	z := e.EncodeAll(b, nil)
	statsEncodeMu.Unlock()
	encoded := `"` + telemetryStatsPrefix + base64.StdEncoding.EncodeToString(z) + `"`
	if len(encoded) >= len(plain) {
		return plain, nil
	}
	return encoded, nil
}

func (s *telemetryStats) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		type legacy telemetryStats
		var old legacy
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		*s = telemetryStats(old)
		return nil
	}
	if len(raw) > base64.StdEncoding.EncodedLen(telemetryStatsLimit)+16 {
		return errors.New("telemetry statistics envelope too large")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	if !strings.HasPrefix(text, telemetryStatsPrefix) {
		return errors.New("unsupported telemetry statistics encoding")
	}
	z, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(text, telemetryStatsPrefix))
	if err != nil {
		return err
	}
	var header zstd.Header
	if err = header.Decode(z); err != nil {
		return err
	}
	if !header.HasFCS || header.Skippable || header.FrameContentSize > telemetryStatsLimit {
		return errors.New("invalid telemetry statistics frame size")
	}
	d, err := statsDecoder()
	if err != nil {
		return err
	}
	statsDecodeMu.Lock()
	b, err := d.DecodeAll(z, make([]byte, 0, int(header.FrameContentSize)))
	statsDecodeMu.Unlock()
	if err != nil {
		return err
	}
	var out telemetryStats
	for _, n := range []*int64{&out.N, &out.Attempts, &out.Loss, &out.FirstNano, &out.LastNano, &out.FirstIngest, &out.LastIngest} {
		v, used := binary.Varint(b)
		if used <= 0 {
			return errors.New("invalid telemetry statistics integer")
		}
		*n, b = v, b[used:]
	}
	for _, n := range []*int{&out.FirstOrdinal, &out.LastOrdinal} {
		v, used := binary.Varint(b)
		if used <= 0 || int64(int(v)) != v {
			return errors.New("invalid telemetry statistics ordinal")
		}
		*n, b = int(v), b[used:]
	}
	for _, f := range []*float64{&out.Sum, &out.Mean, &out.M2, &out.Min, &out.Max, &out.First, &out.Last} {
		if len(b) < 8 {
			return errors.New("truncated telemetry statistics float")
		}
		*f, b = math.Float64frombits(binary.LittleEndian.Uint64(b)), b[8:]
		if math.IsInf(*f, 0) || math.IsNaN(*f) {
			return errors.New("invalid telemetry statistics float")
		}
	}
	if len(b) > 0 {
		// Keep the opaque, CRC-protected digest bytes. Ordinary mean/count
		// charts must not allocate/decode centroids they do not consume.
		// Quantile consumers validate the digest with DecodeTDigest as before.
		out.Digest = append([]byte(nil), b...)
	}
	*s = out
	return nil
}
