//go:build linux

package historyexport

import (
	"os"
	"path/filepath"
	"testing"
)

func groupFixture(t *testing.T, path, cpu, cpuset string) {
	t.Helper()
	if e := os.MkdirAll(path, 0700); e != nil {
		t.Fatal(e)
	}
	for k, v := range map[string]string{"cpu.max": cpu, "cpuset.cpus.effective": cpuset, "memory.max": "134217728", "cpu.max.burst": "0"} {
		if e := os.WriteFile(filepath.Join(path, k), []byte(v), 0600); e != nil {
			t.Fatal(e)
		}
	}
}
func TestResourceProofAncestorAndRuntimeReductions(t *testing.T) {
	root := t.TempDir()
	groupFixture(t, root, "800000 100000", "0-7")
	worker := filepath.Join(root, "exports")
	groupFixture(t, worker, "4000 100000", "0-7")
	instance := filepath.Join(root, "instance")
	groupFixture(t, instance, "max 100000", "0-7")
	for _, cpu := range []string{"50000 100000", "100000 100000", "200000 100000", "400000 100000", "800000 100000"} {
		groupFixture(t, root, cpu, "0-7")
		p, e := VerifyResources(worker, instance)
		if e != nil {
			t.Fatal(cpu, e)
		}
		if p.WorkerCPUs/p.EffectiveCPUs > .1 {
			t.Fatalf("cap exceeded: %+v", p)
		}
	}
	groupFixture(t, root, "800000 100000", "0-1")
	p, e := VerifyResources(worker, instance)
	if e != nil || p.EffectiveCPUs != 2 {
		t.Fatal("cpuset ignored", p, e)
	}
	groupFixture(t, worker, "max 100000", "0-7")
	if _, e = VerifyResources(worker, instance); e == nil {
		t.Fatal("accepted unlimited worker")
	}
	groupFixture(t, worker, "4000 100000", "0-7")
	if e = os.WriteFile(filepath.Join(worker, "cpu.max.burst"), []byte("1000"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = VerifyResources(worker, instance); e == nil {
		t.Fatal("accepted CPU burst")
	}
	groupFixture(t, worker, "4000 100000", "0-7")
	groupFixture(t, root, "25000 100000", "0-7")
	if _, e = VerifyResources(worker, instance); e == nil {
		t.Fatal("accepted unsupported lower capacity")
	}
}
func TestExclusiveWorkerProcessLock(t *testing.T) {
	dir := t.TempDir()
	release, e := lockWorker(dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = lockWorker(dir); e == nil {
		t.Fatal("two workers accepted")
	}
	release()
	release, e = lockWorker(dir)
	if e != nil {
		t.Fatal(e)
	}
	release()
}
