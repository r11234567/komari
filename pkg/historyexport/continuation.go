package historyexport

import (
	"os"
	"reflect"
)

// A live exclusive worker has already hashed every byte it wrote. Rehashing
// gigabytes after every user pause is unnecessary, but a persisted flag is not
// proof: restart, restore, retry and changed files must validate from byte zero.
type checkpointContinuation struct {
	commit Commit
	file   os.FileInfo
}

func (w *Worker) canContinueCheckpoint(j *Job) bool {
	proof, ok := w.continuations[j.ID]
	if !ok || !reflect.DeepEqual(proof.commit, j.Committed) {
		return false
	}
	info, err := os.Lstat(w.Queue.Path(j.ID, ".csv.part"))
	// Sys includes ctime as well as inode/mtime on supported Linux workers.
	// Even a same-size rewrite with restored mtime invalidates the proof.
	return err == nil && info.Mode().IsRegular() && os.SameFile(proof.file, info) && info.Size() == proof.file.Size() && info.ModTime() == proof.file.ModTime() && reflect.DeepEqual(info.Sys(), proof.file.Sys())
}

func (w *Worker) rememberCheckpoint(j *Job) error {
	if j.Committed.Offset == 0 || j.Status == "done" || j.Status == "publishing" || j.Status == "cancelled" || j.Status == "failed" {
		delete(w.continuations, j.ID)
		return nil
	}
	f, err := os.OpenFile(w.Queue.Path(j.ID, ".csv.part"), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	// An interrupted input frame may have appended an uncommitted tail.
	// Remove only that tail before granting the volatile continuation proof.
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < j.Committed.Offset {
		return os.ErrInvalid
	}
	if info.Size() != j.Committed.Offset {
		if err = f.Truncate(j.Committed.Offset); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	info, err = f.Stat()
	if err != nil {
		return err
	}
	if w.continuations == nil {
		w.continuations = map[string]checkpointContinuation{}
	}
	// Eviction only costs a recovery read; it must never retain unbounded
	// metadata for finished, expired or abandoned jobs in a long-lived worker.
	if _, exists := w.continuations[j.ID]; !exists && len(w.continuations) >= 8 {
		clear(w.continuations)
	}
	c := j.Committed
	c.Hash = append([]byte(nil), c.Hash...)
	w.continuations[j.ID] = checkpointContinuation{commit: c, file: info}
	return nil
}
