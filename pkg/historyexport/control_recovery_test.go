package historyexport

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestWorkerControlAtFinalPublicationFence(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			w, j := workerFixture(t, "raw")
			hit := false
			w.Fault = func(stage string) error {
				if stage == "publish-before-rename" {
					hit = true
					return w.Queue.Control(context.Background(), j.Owner, j.ID, action)
				}
				return nil
			}
			if e := w.Step(context.Background(), j); e != nil {
				t.Fatal(e)
			}
			saved, e := w.Queue.Get(context.Background(), j.Owner, j.ID)
			if e != nil {
				t.Fatal(e)
			}
			want := "paused"
			if action == "cancel" {
				want = "cancelled"
			}
			if !hit || saved.Status != want {
				t.Fatalf("final control lost: %+v", saved)
			}
			if _, e = os.Stat(w.Queue.Path(j.ID, ".csv")); !os.IsNotExist(e) {
				t.Fatal("controlled task published", e)
			}
			if action == "pause" {
				w.Fault = nil
				if e = w.Queue.Control(context.Background(), j.Owner, j.ID, "resume"); e != nil {
					t.Fatal(e)
				}
				saved = finishWorker(t, w, saved)
				if len(readCSV(t, w, saved)) != 4 {
					t.Fatal("pause recovery lost rows")
				}
			}
		})
	}
}
func TestWorkerPublicationRejectsLateControl(t *testing.T) {
	w, j := workerFixture(t, "raw")
	hit := false
	w.Fault = func(stage string) error {
		if stage == "publish-after-rename" {
			hit = true
			if e := w.Queue.Control(context.Background(), j.Owner, j.ID, "cancel"); e == nil {
				return errors.New("publication fence accepted late cancellation")
			}
		}
		return nil
	}
	j = finishWorker(t, w, j)
	if !hit || len(readCSV(t, w, j)) != 4 {
		t.Fatal("publication proof missing")
	}
}
func TestWorkerPressurePauseAndHistoryInvalidation(t *testing.T) {
	w, j := workerFixture(t, "raw")
	w.RequirePolicy = true
	p := Policy{Owners: map[string]bool{j.Owner: true}, Nodes: map[string]bool{j.UUID: true}, Pressure: "foreground_latency"}
	if e := WritePolicy(w.Queue.Dir, p); e != nil {
		t.Fatal(e)
	}
	if e := w.Step(context.Background(), j); e != nil {
		t.Fatal(e)
	}
	saved, e := w.Queue.Get(context.Background(), j.Owner, j.ID)
	if e != nil {
		t.Fatal(e)
	}
	if saved.Status != "paused" || saved.WaitReason != "foreground_latency" || saved.DecodedRecords != 0 {
		t.Fatalf("pressure not respected: %+v", saved)
	}
	p.Pressure = ""
	p.State.Epoch = 1
	if e = WritePolicy(w.Queue.Dir, p); e != nil {
		t.Fatal(e)
	}
	if e = w.Step(context.Background(), saved); e != nil {
		t.Fatal(e)
	}
	saved, e = w.Queue.Get(context.Background(), j.Owner, j.ID)
	if e != nil {
		t.Fatal(e)
	}
	if saved.Status != "cancelled" {
		t.Fatal("old history snapshot was reused", saved.Status)
	}
}
func TestPolicyMissingAndExpiredFailClosed(t *testing.T) {
	w, j := workerFixture(t, "raw")
	if reason, e := w.Queue.policy(j); e != nil || reason != "instance_state_unavailable" {
		t.Fatal(reason, e)
	}
	p := Policy{At: time.Now().Add(-time.Minute), Owners: map[string]bool{j.Owner: true}, Nodes: map[string]bool{j.UUID: true}}
	if e := atomicJSON(w.Queue.Dir+"/policy.json", p); e != nil {
		t.Fatal(e)
	}
	if reason, e := w.Queue.policy(j); e != nil || reason != "instance_state_unavailable" {
		t.Fatal(reason, e)
	}
	p.At = time.Now()
	p.Owners = map[string]bool{"someone-else": true}
	if e := atomicJSON(w.Queue.Dir+"/policy.json", p); e != nil {
		t.Fatal(e)
	}
	if _, e := w.Queue.policy(j); !errors.Is(e, errExportInvalidated) {
		t.Fatal("cross-account policy accepted", e)
	}
}
func TestQueueQueuedPauseIsResumableAndCancelIsObservable(t *testing.T) {
	w, j := workerFixture(t, "raw")
	ctx := context.Background()
	if e := w.Queue.Control(ctx, j.Owner, j.ID, "pause"); e != nil {
		t.Fatal(e)
	}
	got, e := w.Queue.Get(ctx, j.Owner, j.ID)
	if e != nil || got.Status != "paused" {
		t.Fatal(got, e)
	}
	if e = w.Queue.Control(ctx, j.Owner, j.ID, "resume"); e != nil {
		t.Fatal(e)
	}
	if e = w.Queue.Control(ctx, j.Owner, j.ID, "cancel"); e != nil {
		t.Fatal(e)
	}
	got, e = w.Queue.Get(ctx, j.Owner, j.ID)
	if e != nil || got.Status != "cancel_requested" {
		t.Fatal(got, e)
	}
	if e = w.Step(ctx, got); e != nil {
		t.Fatal(e)
	}
	got, e = w.Queue.Get(ctx, j.Owner, j.ID)
	if e != nil || got.Status != "cancelled" {
		t.Fatal(got, e)
	}
}
