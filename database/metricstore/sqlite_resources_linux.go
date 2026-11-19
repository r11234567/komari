//go:build linux

package metricstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func detectEffectiveResources() (int64, int) {
	memoryBytes := linuxPhysicalMemory()
	paths := []string{"/sys/fs/cgroup/memory/memory.limit_in_bytes"}
	for _, dir := range linuxCgroupV2Directories() {
		paths = append(paths, filepath.Join(dir, "memory.max"))
	}
	for _, path := range paths {
		if limit := readPositiveResourceNumber(path); limit > 0 && (memoryBytes <= 0 || limit < memoryBytes) {
			memoryBytes = limit
		}
	}
	cpuCount := runtime.NumCPU()
	if quota := linuxCPUQuota(); quota > 0 && quota < cpuCount {
		cpuCount = quota
	}
	if cpuset := linuxCPUSetCount(); cpuset > 0 && cpuset < cpuCount {
		cpuCount = cpuset
	}
	return memoryBytes, cpuCount
}

func linuxPhysicalMemory() int64 {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0
	}
	return int64(info.Totalram) * int64(info.Unit)
}

func readPositiveResourceNumber(path string) int64 {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value := strings.TrimSpace(string(content))
	if value == "" || value == "max" {
		return 0
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || n >= 1<<60 {
		return 0
	}
	return n
}

func linuxCPUQuota() int {
	count := 0
	for _, dir := range linuxCgroupV2Directories() {
		if content, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
			fields := strings.Fields(string(content))
			if len(fields) == 2 && fields[0] != "max" {
				quota, quotaErr := strconv.ParseInt(fields[0], 10, 64)
				period, periodErr := strconv.ParseInt(fields[1], 10, 64)
				if quotaErr == nil && periodErr == nil && quota > 0 && period > 0 {
					n := int(1 + (quota-1)/period)
					if count == 0 || n < count {
						count = n
					}
				}
			}
		}
	}
	if count > 0 {
		return count
	}
	quota := readPositiveResourceNumber("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	period := readPositiveResourceNumber("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if quota > 0 && period > 0 {
		return int((quota + period - 1) / period)
	}
	return 0
}

// A container commonly mounts its own cgroup as the root; systemd services
// commonly see the whole hierarchy. Honor both layouts and inherited limits.
func linuxCgroupV2Directories() []string {
	b, _ := os.ReadFile("/proc/self/cgroup")
	return cgroupV2Directories(string(b))
}

func cgroupV2Directories(membership string) []string {
	const root = "/sys/fs/cgroup"
	dirs := []string{root}
	for _, line := range strings.Split(membership, "\n") {
		if !strings.HasPrefix(line, "0::/") {
			continue
		}
		rel := strings.TrimPrefix(line, "0::/")
		if rel == "" || !filepath.IsLocal(rel) {
			return dirs
		}
		path := filepath.Join(root, rel)
		for path != root && len(dirs) < 64 {
			dirs = append(dirs, path)
			path = filepath.Dir(path)
		}
		break
	}
	return dirs
}

func linuxCPUSetCount() int {
	paths := []string{"/sys/fs/cgroup/cpuset/cpuset.cpus"}
	for _, dir := range linuxCgroupV2Directories() {
		paths = append(paths, filepath.Join(dir, "cpuset.cpus.effective"))
	}
	minimum := 0
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if count := countCPUSet(strings.TrimSpace(string(content))); count > 0 && (minimum == 0 || count < minimum) {
			minimum = count
		}
	}
	return minimum
}

func countCPUSet(value string) int {
	total := 0
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bounds := strings.SplitN(part, "-", 2)
		first, err := strconv.Atoi(bounds[0])
		if err != nil || first < 0 {
			return 0
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(bounds[1])
			if err != nil || last < first {
				return 0
			}
		}
		total += last - first + 1
	}
	return total
}
