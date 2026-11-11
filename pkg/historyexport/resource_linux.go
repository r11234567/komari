//go:build linux

package historyexport

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type ResourceProof struct {
	At             string  `json:"at"`
	EffectiveCPUs  float64 `json:"effective_cpus"`
	WorkerCPUs     float64 `json:"worker_cpus"`
	Quota          int64   `json:"quota_us"`
	Period         int64   `json:"period_us"`
	MemoryMax      int64   `json:"memory_max"`
	Cgroup         string  `json:"cgroup"`
	InstanceCgroup string  `json:"instance_cgroup"`
}

func SelfCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, s := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(s, "0::") {
			p := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(strings.TrimPrefix(s, "0::"), "/"))
			if _, err = os.Stat(filepath.Join(p, "cpu.max")); err == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("cgroup v2 CPU controller is unavailable")
}
func quota(path string) (int64, int64, error) {
	b, e := os.ReadFile(filepath.Join(path, "cpu.max"))
	if e != nil {
		return 0, 0, e
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, 0, errors.New("invalid cpu.max")
	}
	period, e := strconv.ParseInt(parts[1], 10, 64)
	if e != nil || period <= 0 {
		return 0, 0, errors.New("invalid CPU period")
	}
	if parts[0] == "max" {
		return -1, period, nil
	}
	q, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || q < 1000 {
		return 0, 0, errors.New("invalid CPU quota")
	}
	return q, period, nil
}
func cpuCount(text string) (int, error) {
	var n int
	for _, item := range strings.Split(strings.TrimSpace(text), ",") {
		parts := strings.Split(item, "-")
		a, e := strconv.Atoi(parts[0])
		if e != nil || a < 0 {
			return 0, errors.New("invalid cpuset")
		}
		b := a
		if len(parts) == 2 {
			b, e = strconv.Atoi(parts[1])
			if e != nil || b < a {
				return 0, errors.New("invalid cpuset range")
			}
		} else if len(parts) > 2 {
			return 0, errors.New("invalid cpuset")
		}
		n += b - a + 1
	}
	return n, nil
}

// Effective capacity includes every visible ancestor. Deployment supplies the
// instance path, never the export child's own reduced quota as the denominator.
func effectiveCPU(path string) (float64, error) {
	capacity := math.Inf(1)
	seen := false
	for {
		q, p, e := quota(path)
		if e == nil {
			seen = true
			if q > 0 {
				capacity = math.Min(capacity, float64(q)/float64(p))
			}
		} else if !os.IsNotExist(e) {
			return 0, e
		}
		if b, e := os.ReadFile(filepath.Join(path, "cpuset.cpus.effective")); e == nil && len(strings.TrimSpace(string(b))) > 0 {
			n, e := cpuCount(string(b))
			if e != nil {
				return 0, e
			}
			capacity = math.Min(capacity, float64(n))
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	if !seen || math.IsInf(capacity, 1) || capacity <= 0 {
		return 0, errors.New("cannot prove instance CPU capacity")
	}
	return capacity, nil
}

// A conservative .04-core group cap remains below 10% throughout the supported
// .5–8 CPU range, including runtime reductions across that range. With at most
// 100ms periods and zero burst its 1-second boundary allowance is < .05 core.
// This deliberately leaves throughput on the table; no idle-CPU acceleration.
func VerifyResources(worker, instance string) (ResourceProof, error) {
	p := ResourceProof{Cgroup: worker, InstanceCgroup: instance}
	if !filepath.IsAbs(worker) || !filepath.IsAbs(instance) || filepath.Clean(worker) == filepath.Clean(instance) || strings.HasPrefix(filepath.Clean(instance)+string(os.PathSeparator), filepath.Clean(worker)+string(os.PathSeparator)) {
		return p, errors.New("export worker requires its own cgroup and an explicit distinct instance cgroup")
	}
	if info,e:=os.Stat(instance);e!=nil || !info.IsDir(){return p,errors.New("configured instance cgroup does not exist")}
	q, period, e := quota(worker)
	if e != nil {
		return p, e
	}
	p.Quota = q
	p.Period = period
	p.WorkerCPUs = float64(q) / float64(period)
	if q < 1000 || period > 100000 || p.WorkerCPUs > .040000001 {
		return p, errors.New("strict export worker requires cpu.max <= 0.04 core and period <= 100ms")
	}
	if b, e := os.ReadFile(filepath.Join(worker, "cpu.max.burst")); e == nil {
		v, e := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if e != nil || v != 0 {
			return p, errors.New("export CPU burst must be zero")
		}
	} else if !os.IsNotExist(e) {
		return p, e
	}
	p.EffectiveCPUs, e = effectiveCPU(instance)
	if e != nil {
		return p, e
	}
	if p.EffectiveCPUs < .5 {
		return p, errors.New("strict export needs at least 0.5 effective instance CPU; pause and reconfigure before reducing below this floor")
	}
	b, e := os.ReadFile(filepath.Join(worker, "memory.max"))
	if e != nil {
		return p, e
	}
	p.MemoryMax, e = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if e != nil || p.MemoryMax <= 0 || p.MemoryMax > 128<<20 {
		return p, errors.New("export worker requires memory.max <= 128 MiB")
	}
	return p, nil
}
func lockWorker(dir string) (func(), error) {
	f, e := os.OpenFile(filepath.Join(dir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("another export worker owns this queue: %w", e)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
func diskAvailable(path string) (uint64, error) {
	var s syscall.Statfs_t
	if e := syscall.Statfs(path, &s); e != nil {
		return 0, e
	}
	return s.Bavail * uint64(s.Bsize), nil
}

func processIOBytes() (uint64, error) {
	b, e := os.ReadFile("/proc/self/io")
	if e != nil {
		return 0, e
	}
	var total uint64
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && (parts[0] == "rchar:" || parts[0] == "wchar:") {
			n, e := strconv.ParseUint(parts[1], 10, 64)
			if e != nil {
				return 0, e
			}
			total += n
		}
	}
	return total, nil
}
