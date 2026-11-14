package metric

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// V3 uses byte-sized ordinary JSONL/zstd frames, each with zstd's checksum.
// A single small CRC protects the skippable selection index at EOF. V2 remains
// readable; no new per-frame SHA or duplicated header/footer is written.
const telemetryFramePlainLimit = 1 << 20
const telemetryIndexLimit = 2 << 20
const telemetryIndexMagic = "KOMIDX02"
const telemetryIndexMagicV3 = "KOMIDX03"

type telemetryArchiveFrame struct {
	Offset, Bytes, Points, First, Last int64
	Hash                               string `json:",omitempty"` // v2 compatibility only
	Version                            int    `json:"v,omitempty"`
	Entity                             string `json:"e,omitempty"`
	ObservedFirst, ObservedLast        int64  `json:",omitempty"`
}
type telemetryArchiveIndex struct {
	Version int
	Segment string
	Frames  []telemetryArchiveFrame
}
type telemetryFrameWriter struct {
	out                                              *telemetryHashWriter
	encoder                                          *zstd.Encoder
	index                                            telemetryArchiveIndex
	plain                                            bytes.Buffer
	points, first, last, observedFirst, observedLast int64
	entity                                           string
	totalPlain                                       int64
}

func newTelemetryFrameWriter(out *telemetryHashWriter, id string) (*telemetryFrameWriter, error) {
	enc, e := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<20))
	if e != nil {
		return nil, e
	}
	return &telemetryFrameWriter{out: out, encoder: enc, index: telemetryArchiveIndex{Version: 3, Segment: id}}, nil
}
func (w *telemetryFrameWriter) add(r telemetryRecord) error {
	if r.Kind != "point" || r.Point == nil {
		return errors.New("archive requires a point record")
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) >= 64<<10 {
		return errors.New("archive record exceeds 64 KiB")
	}
	if w.points > 0 && (w.plain.Len()+len(b)+1 > telemetryFramePlainLimit || w.entity != r.Point.EntityID) {
		if e = w.flush(); e != nil {
			return e
		}
	}
	w.plain.Write(b)
	w.plain.WriteByte('\n')
	ns := r.Point.Timestamp.UnixNano()
	if w.points == 0 {
		w.first, w.entity, w.observedFirst, w.observedLast = r.Ingest, r.Point.EntityID, ns, ns
	}
	w.last = r.Ingest
	w.observedFirst, w.observedLast = min(w.observedFirst, ns), max(w.observedLast, ns)
	w.points++
	w.totalPlain += int64(len(b) + 1)
	return nil
}
func (w *telemetryFrameWriter) flush() error {
	if w.points == 0 {
		return nil
	}
	compressed := w.encoder.EncodeAll(w.plain.Bytes(), nil)
	frame := telemetryArchiveFrame{Offset: w.out.n, Bytes: int64(len(compressed)), Points: w.points, First: w.first, Last: w.last, Version: 3, Entity: w.entity, ObservedFirst: w.observedFirst, ObservedLast: w.observedLast}
	if _, e := w.out.Write(compressed); e != nil {
		return e
	}
	w.index.Frames = append(w.index.Frames, frame)
	if len(w.index.Frames) > 16384 {
		return errors.New("archive frame index exceeds budget")
	}
	w.plain.Reset()
	w.points = 0
	return nil
}
func (w *telemetryFrameWriter) close() error {
	defer w.encoder.Close()
	if e := w.flush(); e != nil {
		return e
	}
	b, e := json.Marshal(w.index)
	if e != nil {
		return e
	}
	if len(b) > telemetryIndexLimit {
		return errors.New("archive frame index exceeds budget")
	}
	trailer := make([]byte, 16)
	binary.LittleEndian.PutUint32(trailer[:4], uint32(len(b)))
	binary.LittleEndian.PutUint32(trailer[4:8], crc32.ChecksumIEEE(b))
	copy(trailer[8:], telemetryIndexMagicV3)
	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header, 0x184d2a50)
	binary.LittleEndian.PutUint32(header[4:], uint32(len(b)+16))
	for _, part := range [][]byte{header, b, trailer} {
		if _, e = w.out.Write(part); e != nil {
			return e
		}
	}
	return nil
}
func readTelemetryArchiveIndex(f *os.File, seg telemetrySegment) (*telemetryArchiveIndex, error) {
	if seg.FileBytes < 24 {
		return nil, nil
	}
	trailer := make([]byte, 16)
	if _, e := f.ReadAt(trailer, seg.FileBytes-16); e != nil {
		return nil, e
	}
	magic := string(trailer[8:])
	if magic != telemetryIndexMagic && magic != telemetryIndexMagicV3 {
		return nil, nil
	}
	n := int64(binary.LittleEndian.Uint64(trailer))
	if magic == telemetryIndexMagicV3 {
		n = int64(binary.LittleEndian.Uint32(trailer[:4]))
	}
	if n < 1 || n > telemetryIndexLimit || n+24 > seg.FileBytes {
		return nil, errors.New("invalid archive index size")
	}
	offset := seg.FileBytes - n - 24
	b := make([]byte, n+8)
	if _, e := f.ReadAt(b, offset); e != nil {
		return nil, e
	}
	if binary.LittleEndian.Uint32(b) != 0x184d2a50 || int64(binary.LittleEndian.Uint32(b[4:])) != n+16 {
		return nil, errors.New("invalid archive index framing")
	}
	if magic == telemetryIndexMagicV3 && crc32.ChecksumIEEE(b[8:]) != binary.LittleEndian.Uint32(trailer[4:8]) {
		return nil, errors.New("archive index checksum mismatch")
	}
	var index telemetryArchiveIndex
	if e := json.Unmarshal(b[8:], &index); e != nil {
		return nil, e
	}
	if (index.Version != 2 && index.Version != 3) || (index.Version == 3 && magic != telemetryIndexMagicV3) || index.Segment != seg.ID || len(index.Frames) > 16384 {
		return nil, errors.New("invalid archive index identity")
	}
	var pos, count, after int64
	for _, frame := range index.Frames {
		if frame.Offset != pos || frame.Bytes < 1 || frame.Bytes > 2<<20 || frame.Points < 1 || frame.Points > 100000 || frame.First < seg.First || frame.Last > seg.Last || frame.First > frame.Last || frame.First < after || (index.Version == 2 && (frame.Points > 128 || len(frame.Hash) != 64)) || (index.Version == 3 && (frame.Version != 3 || frame.ObservedFirst > frame.ObservedLast)) {
			return nil, errors.New("invalid archive frame extent")
		}
		pos += frame.Bytes
		count += frame.Points
		after = frame.Last
	}
	if pos != offset || count != seg.Points {
		return nil, errors.New("archive index coverage mismatch")
	}
	return &index, nil
}
func verifyTelemetryArchiveFile(ctx context.Context, f *os.File, seg telemetrySegment) error {
	h := sha256.New()
	n, e := io.Copy(h, &telemetryContextReader{ctx: ctx, r: io.NewSectionReader(f, 0, seg.FileBytes)})
	if e != nil {
		return e
	}
	if n != seg.FileBytes || hex.EncodeToString(h.Sum(nil)) != seg.Hash {
		return errors.New("archive compressed checksum mismatch")
	}
	return nil
}
