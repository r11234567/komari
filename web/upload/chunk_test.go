package upload

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestChunksRetryOwnershipAndOrderedMerge(t *testing.T) {
	s := NewStore(t.TempDir())
	first := bytes.Repeat([]byte("a"), int(ChunkSize))
	last := []byte("last")
	session, err := s.Init("owner", PurposePlugin, "plugin.zip", ChunkSize+int64(len(last)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChunk("other", session.ID, 0, bytes.NewReader(first)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross owner: %v", err)
	}
	if err := s.Cancel("other", session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross cancel: %v", err)
	}
	if err := s.SaveChunk("owner", session.ID, 2, bytes.NewReader(last)); err == nil {
		t.Fatal("invalid index")
	}
	if err := s.SaveChunk("owner", session.ID, 1, bytes.NewReader(last[:1])); err == nil {
		t.Fatal("short chunk")
	}
	if err := s.SaveChunk("owner", session.ID, 1, bytes.NewReader(last)); err != nil {
		t.Fatal(err)
	}
	called := false
	finish := func(session Session) (Result, error) {
		called = true
		data, err := os.ReadFile(session.ArchivePath)
		if err != nil {
			return Result{}, err
		}
		if !bytes.Equal(data, append(first, last...)) {
			t.Fatal("merge content/order mismatch")
		}
		return Result{Message: "done"}, nil
	}
	if _, err := s.Finalize("owner", session.ID, finish); err == nil || called {
		t.Fatal("missing chunk finalized")
	}
	for range 2 {
		if err := s.SaveChunk("owner", session.ID, 0, bytes.NewReader(first)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Finalize("owner", session.ID, finish)
	if err != nil || result.Message != "done" || !called {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := os.Stat(session.Directory); !os.IsNotExist(err) {
		t.Fatal("finalized archive retained")
	}
	if _, err := s.Finalize("owner", session.ID, finish); !errors.Is(err, ErrNotFound) {
		t.Fatal("duplicate finalize accepted")
	}
}

func TestUploadLimitsExpirationAndCancellation(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	s.now = func() time.Time { return now }
	for _, size := range []int64{0, -1, 100<<20 + 1} {
		if _, err := s.Init("owner", PurposePlugin, "p.zip", size); err == nil {
			t.Fatalf("size %d accepted", size)
		}
	}
	if _, err := s.Init("owner", "unknown", "p.zip", 1); err == nil {
		t.Fatal("unknown purpose")
	}
	a, err := s.Init("owner", PurposeBackup, "b.zip", 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Init("owner", PurposeBackup, "b.zip", 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Init("other", PurposePlugin, "p.zip", 1); err == nil {
		t.Fatal("global quota exceeded")
	}
	if err := s.Cancel("owner", b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Init("owner", PurposePlugin, "p.zip", 1); err != nil {
		t.Fatal("cancel did not free quota")
	}
	now = now.Add(Lifetime)
	s.Sweep()
	if err := s.SaveChunk("owner", a.ID, 0, bytes.NewReader(nil)); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired upload accepted")
	}
	if _, err := os.Stat(a.Directory); !os.IsNotExist(err) {
		t.Fatal("expired files retained")
	}
	if err := s.Cancel("owner", "../outside"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unsafe ID")
	}
}

func TestFinalizeSerializesCancelAndCleansUpFailures(t *testing.T) {
	s := NewStore(t.TempDir())
	item, err := s.Init("owner", PurposePlugin, "p.zip", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChunk("owner", item.ID, 0, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.Finalize("owner", item.ID, func(session Session) (Result, error) {
			close(started)
			<-release
			if _, err := os.Stat(session.ArchivePath); err != nil {
				t.Error("cancel removed active archive")
			}
			return Result{}, io.ErrUnexpectedEOF
		})
	}()
	<-started
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.Cancel("owner", item.ID) }()
	close(release)
	wg.Wait()
	if _, err := os.Stat(item.Directory); !os.IsNotExist(err) {
		t.Fatal("failed finalizer leaked files")
	}
}

func TestRestartOnlyRemovesOwnedOrphanDirectories(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "upload-old")
	if err := os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(root)
	if _, err := s.Init("owner", PurposePlugin, "p.zip", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("orphan retained")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("unrelated file removed")
	}
}
