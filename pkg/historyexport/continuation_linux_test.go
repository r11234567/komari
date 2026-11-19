//go:build linux

package historyexport

import (
	"context"
	"crypto/sha256"
	"encoding"
	"os"
	"testing"
	"time"
)

// Reproduce the failed qualification's ~26 MB committed prefix at its actual
// 128 KiB/s logical-read budget. This is finite, not a local long soak. The
// normal continuation must publish without re-reading it; a fresh worker must
// start visible byte-zero verification and remain interruptible.
func TestLiveResumeLargePrefixAtQualificationBudget(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-worker", true: "fresh-worker"}[restart], func(t *testing.T) {
			w, j := pauseAtPublication(t, "raw")
			path := w.Queue.Path(j.ID, ".csv.part")
			f, err := os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0600)
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			buf := make([]byte, 64<<10)
			for i := range buf {
				buf[i] = 'x'
			}
			for i := 0; i < 400; i++ {
				if _, err = f.Write(buf); err != nil {
					t.Fatal(err)
				}
				h.Write(buf)
			}
			if err = f.Sync(); err != nil {
				t.Fatal(err)
			}
			f.Close()
			j.Committed.Offset = 400 * int64(len(buf))
			j.Committed.Hash, err = h.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			j.OutputBytes = j.Committed.Offset
			if err = w.Queue.Save(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			if err = w.rememberCheckpoint(j); err != nil {
				t.Fatal(err)
			}
			if restart {
				w = &Worker{Queue: w.Queue, Verify: w.Verify}
			}
			w.io = &ioBudget{rate: 131072}
			w.Fault = nil
			if err = w.Queue.Control(context.Background(), j.Owner, j.ID, "resume"); err != nil {
				t.Fatal(err)
			}
			j, err = w.Queue.Next(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			validated := false
			w.Fault = func(stage string) error {
				if stage == "checkpoint-validation-progress" {
					validated = true
					if !restart {
						t.Fatal("live continuation re-read 26 MB")
					}
					if j.Status != "recovering" || j.Verification == nil || j.Verification.VerifiedBytes != 0 {
						t.Fatal("restart skipped visible byte-zero recovery")
					}
					cancel()
				}
				return nil
			}
			started := time.Now()
			err = w.Step(ctx, j)
			if restart {
				if !validated || ctx.Err() == nil {
					t.Fatal("restart trusted volatile proof", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				j, err = w.Queue.Get(context.Background(), j.Owner, j.ID)
				if err != nil || j.Status != "done" {
					t.Fatal("live continuation stalled", j.Status, err)
				}
			}
			t.Logf("restart=%t prefix_bytes=%d elapsed=%s verification=%t", restart, j.Committed.Offset, time.Since(started), validated)
		})
	}
}
