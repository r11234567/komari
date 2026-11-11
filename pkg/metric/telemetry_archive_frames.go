package metric

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// Version 2 is a sequence of independently checksummed JSONL/zstd frames.
// Its bounded index lives in one standard zstd skippable frame at EOF. Backups
// copy the single immutable file unchanged; old one-stream archives stay valid.
const telemetryFramePlainLimit = 1 << 20
const telemetryIndexLimit = 2 << 20
const telemetryIndexMagic = "KOMIDX02"

type telemetryArchiveFrame struct {
	Offset, Bytes, Points, First, Last int64
	Hash                               string
}
type telemetryArchiveIndex struct {
	Version int
	Segment string
	Frames  []telemetryArchiveFrame
}
type telemetryFrameWriter struct {
	out        *telemetryHashWriter
	encoder    *zstd.Encoder
	index      telemetryArchiveIndex
	records    []telemetryRecord
	plainBytes int
	totalPlain int64
}

func newTelemetryFrameWriter(out *telemetryHashWriter, id string) (*telemetryFrameWriter, error) {
	enc, e := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<20))
	if e != nil {
		return nil, e
	}
	return &telemetryFrameWriter{out: out, encoder: enc, index: telemetryArchiveIndex{Version: 2, Segment: id}}, nil
}
func (w *telemetryFrameWriter) add(r telemetryRecord) error {
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) >= 64<<10 {
		return errors.New("archive record exceeds 64 KiB")
	}
	if len(w.records) > 0 && (w.plainBytes+len(b)+1 > telemetryFramePlainLimit || len(w.records) >= 128) {
		if e = w.flush(); e != nil {
			return e
		}
	}
	w.records = append(w.records, r)
	w.plainBytes += len(b) + 1
	w.totalPlain += int64(len(b) + 1)
	return nil
}
func (w *telemetryFrameWriter) flush() error {
	if len(w.records) == 0 {
		return nil
	}
	var plain bytes.Buffer
	enc := json.NewEncoder(&plain)
	if e := enc.Encode(telemetryRecord{Kind: "header", Version: 1, Segment: w.index.Segment}); e != nil {
		return e
	}
	for _, r := range w.records {
		if e := enc.Encode(r); e != nil {
			return e
		}
	}
	h := sha256.Sum256(plain.Bytes())
	if e := enc.Encode(telemetryRecord{Kind: "footer", Count: int64(len(w.records)), Hash: hex.EncodeToString(h[:])}); e != nil {
		return e
	}
	compressed := w.encoder.EncodeAll(plain.Bytes(), nil)
	sum := sha256.Sum256(compressed)
	frame := telemetryArchiveFrame{Offset: w.out.n, Bytes: int64(len(compressed)), Points: int64(len(w.records)), First: w.records[0].Ingest, Last: w.records[len(w.records)-1].Ingest, Hash: hex.EncodeToString(sum[:])}
	if _, e := w.out.Write(compressed); e != nil {
		return e
	}
	w.index.Frames = append(w.index.Frames, frame)
	if len(w.index.Frames) > 16384 {
		return errors.New("archive frame index exceeds budget")
	}
	w.records = nil
	w.plainBytes = 0
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
	binary.LittleEndian.PutUint64(trailer[:8], uint64(len(b)))
	copy(trailer[8:], telemetryIndexMagic)
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
	if string(trailer[8:]) != telemetryIndexMagic {
		return nil, nil
	}
	n := int64(binary.LittleEndian.Uint64(trailer))
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
	var index telemetryArchiveIndex
	if e := json.Unmarshal(b[8:], &index); e != nil {
		return nil, e
	}
	if index.Version != 2 || index.Segment != seg.ID || len(index.Frames) > 16384 {
		return nil, errors.New("invalid archive index identity")
	}
	var pos, count, after int64
	for _, frame := range index.Frames {
		if frame.Offset != pos || frame.Bytes < 1 || frame.Bytes > 2<<20 || frame.Points < 1 || frame.Points > 128 || frame.First < seg.First || frame.Last > seg.Last || frame.First > frame.Last || frame.First < after || len(frame.Hash) != 64 {
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
