package historyexport

import (
	"os"
	"reflect"

	"github.com/komari-monitor/komari/pkg/metric"
)

type cachedExportSnapshot struct {
	snapshot   metric.ExportSnapshot
	file       os.FileInfo
	generation string
	inputBytes []int64
}

func sameSnapshotFile(a, b os.FileInfo) bool {
	return b.Mode().IsRegular() && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime() && reflect.DeepEqual(a.Sys(), b.Sys())
}

func (w *Worker) cacheSnapshot(j *Job, path string, snapshot metric.ExportSnapshot) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if w.snapshots == nil {
		w.snapshots = make(map[string]cachedExportSnapshot)
	}
	// The durable queue permits at most eight live jobs. Finished jobs release
	// their entries; an expired/replaced job can only cost a one-time reload.
	if len(w.snapshots) >= 8 {
		for id := range w.snapshots {
			delete(w.snapshots, id)
			break
		}
	}
	prefix := make([]int64, len(snapshot.Units)+1)
	for i, u := range snapshot.Units {
		prefix[i+1] = prefix[i] + u.Segment.FileBytes
	}
	w.snapshots[j.ID] = cachedExportSnapshot{snapshot, info, j.Generation, prefix}
	return nil
}

func (w *Worker) snapshotInputBytes(id string, completed int) int64 {
	c, ok := w.snapshots[id]
	if !ok || completed <= 0 {
		return 0
	}
	completed = min(completed, len(c.inputBytes)-1)
	return c.inputBytes[completed]
}
