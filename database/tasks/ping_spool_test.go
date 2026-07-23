package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadPingSpoolBatchResumesAtLineBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segment.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		_, err = fmt.Fprintf(file, "{\"client\":\"node\",\"task_id\":1,\"time\":%q,\"value\":%d}\n", time.Unix(int64(index), 0).UTC().Format(time.RFC3339Nano), index)
		if err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	first, offset, done, err := readPingSpoolBatch(path, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || done {
		t.Fatalf("first batch length = %d, done = %v", len(first), done)
	}
	second, _, done, err := readPingSpoolBatch(path, offset, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || !done || second[0].Value != 2 {
		t.Fatalf("second batch = %#v, done = %v", second, done)
	}
}
