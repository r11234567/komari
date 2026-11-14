//go:build linux

package historyexport

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/csv"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mincore observes residency without faulting pages into the process. A small
// Go heap alone cannot detect output cache charged to the worker's cgroup.
func residentFileBytes(t *testing.T, f *os.File) int64 {
	t.Helper()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		return 0
	}
	pages := (info.Size() + int64(os.Getpagesize()) - 1) / int64(os.Getpagesize())
	view, err := unix.Mmap(int(f.Fd()), 0, int(info.Size()), unix.PROT_NONE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(view)
	residency := make([]byte, int(pages))
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&view[0])), uintptr(len(view)), uintptr(unsafe.Pointer(&residency[0])))
	if errno != 0 {
		t.Fatal(errno)
	}
	var resident int64
	for _, page := range residency {
		if page&1 != 0 {
			resident += int64(os.Getpagesize())
		}
	}
	return resident
}

func TestOutputAndRecoveryBoundFileCache(t *testing.T) {
	w, j := workerFixture(t, "raw")
	ctx := context.Background()
	o, csvWriter, err := w.output(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	defer o.f.Close()
	row := []string{"sequential-export", strings.Repeat("abcdefgh", 1024)}
	const rows = 32768 // >256 MiB, even though Go retains only one row.
	var peak int64
	for i := 0; i < rows; i++ {
		if err = csvWriter.Write(row); err != nil {
			t.Fatal(err)
		}
		j.WrittenRows++
		if i%1024 == 1023 {
			csvWriter.Flush()
			if err = csvWriter.Error(); err != nil {
				t.Fatal(err)
			}
			resident := residentFileBytes(t, o.f)
			peak = max(peak, resident)
			if resident > 8<<20 {
				t.Fatalf("CSV page cache grows within one input: %d bytes resident after %d output bytes", resident, o.n)
			}
		}
	}
	if err = w.checkpoint(ctx, j, o, csvWriter, 0); err != nil {
		t.Fatal(err)
	}
	if resident := residentFileBytes(t, o.f); resident > int64(os.Getpagesize()) {
		t.Fatalf("checkpoint retained %d bytes of synced output cache", resident)
	}
	if err = o.f.Close(); err != nil {
		t.Fatal(err)
	}
	// Restore the durable prefix through the actual recovery reader. Its hash
	// must match and its sequential read must release the consumed cache too.
	j.Status = "recovering"
	if err = w.verifyCheckpoint(ctx, j); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(w.Queue.Path(j.ID, ".csv.part"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if resident := residentFileBytes(t, f); resident > 2<<20 {
		t.Fatalf("checkpoint verification retained %d bytes of read cache", resident)
	}
	if err = limitFileReadAhead(f); err != nil {
		t.Fatal(err)
	}
	// Independently validate persisted CSV semantics and its bytes, using a
	// streaming reader after memory measurements so this read cannot warm them.
	h := sha256.New()
	r := csv.NewReader(io.TeeReader(f, h))
	var validatedCache int64
	for i := 0; i < rows; i++ {
		got, err := r.Read()
		if err != nil || len(got) != 2 || got[0] != row[0] || got[1] != row[1] {
			t.Fatalf("persisted CSV row %d changed: %v", i, err)
		}
		if i%128 == 127 {
			position, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				t.Fatal(err)
			}
			if resident := residentFileBytes(t, f); resident > 8<<20 {
				t.Fatalf("independent CSV validation warmed %d bytes", resident)
			}
			validatedCache, err = releaseFileCache(f, validatedCache, position)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = r.Read(); err != io.EOF {
		t.Fatalf("unexpected CSV tail: %v", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(o.h.Sum(nil)) {
		t.Fatal("cache release changed durable output bytes")
	}
	t.Logf("rows=%d output_bytes=%d peak_output_file_cache_bytes=%d", rows, o.n, peak)
}

func TestReleaseFileCachePreservesUnalignedBytes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "unaligned")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := strings.Repeat("x", os.Getpagesize()*3+17)
	if _, err = f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	released, err := releaseFileCache(f, 13, int64(len(data)))
	if err != nil || released != int64(os.Getpagesize()*3) {
		t.Fatalf("unaligned cache boundary: %d %v", released, err)
	}
	got := make([]byte, len(data))
	if _, err = f.ReadAt(got, 0); err != nil || string(got) != data {
		t.Fatalf("cache release changed bytes: %v", err)
	}
}

func TestRecoveryReleasesConsumedFileCache(t *testing.T) {
	w, j := workerFixture(t, "raw")
	f, err := os.Create(w.Queue.Path(j.ID, ".csv.part"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	block := []byte(strings.Repeat("recovery", 8192))
	const blocks = 2048 // 128 MiB of committed output, created without CSV code.
	var fixtureCache int64
	for i := 0; i < blocks; i++ {
		if _, err = f.Write(block); err != nil {
			t.Fatal(err)
		}
		h.Write(block)
		if i%64 == 63 {
			if err = f.Sync(); err != nil {
				t.Fatal(err)
			}
			fixtureCache, err = releaseFileCache(f, fixtureCache, int64(len(block)*(i+1)))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	j.Committed.Offset = int64(len(block) * blocks)
	j.Committed.Hash, err = h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = releaseFileCache(f, 0, j.Committed.Offset); err != nil {
		t.Fatal(err)
	}
	j.Status = "recovering"
	if err = w.verifyCheckpoint(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if resident := residentFileBytes(t, f); resident > 2<<20 {
		t.Fatalf("recovery accumulated %d bytes of consumed read cache", resident)
	}
}
