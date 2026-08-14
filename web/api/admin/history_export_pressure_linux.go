//go:build linux

package admin

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

type historyExportSystemSample struct {
	cpuTotal        uint64
	cpuIdle         uint64
	loadRatio       float64
	memoryTotal     uint64
	memoryAvailable uint64
}

func (sample historyExportSystemSample) memoryAvailableRatio() float64 {
	if sample.memoryTotal == 0 {
		return 0
	}
	return float64(sample.memoryAvailable) / float64(sample.memoryTotal)
}

func sampleHistoryExportSystem() historyExportSystemSample {
	sample := historyExportSystemSample{loadRatio: -1}
	if content, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(content), "\n")
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[0] == "cpu" {
			for index, field := range fields[1:] {
				value, parseErr := strconv.ParseUint(field, 10, 64)
				if parseErr != nil {
					break
				}
				sample.cpuTotal += value
				if index == 3 || index == 4 {
					sample.cpuIdle += value
				}
			}
		}
	}
	if content, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(content))
		if len(fields) > 0 {
			if load, parseErr := strconv.ParseFloat(fields[0], 64); parseErr == nil {
				cpus := runtime.NumCPU()
				if cpus < 1 {
					cpus = 1
				}
				sample.loadRatio = load / float64(cpus)
				if sample.loadRatio > 1 {
					sample.loadRatio = 1
				}
			}
		}
	}
	if content, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			value, parseErr := strconv.ParseUint(fields[1], 10, 64)
			if parseErr != nil {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				sample.memoryTotal = value * 1024
			case "MemAvailable:":
				sample.memoryAvailable = value * 1024
			}
		}
	}
	applyHistoryExportCgroupMemory(&sample)
	return sample
}

func applyHistoryExportCgroupMemory(sample *historyExportSystemSample) {
	for _, paths := range [][2]string{
		{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.current"},
		{"/sys/fs/cgroup/memory/memory.limit_in_bytes", "/sys/fs/cgroup/memory/memory.usage_in_bytes"},
	} {
		limit := readHistoryExportUintFile(paths[0])
		used := readHistoryExportUintFile(paths[1])
		if limit == 0 || used > limit || limit >= 1<<60 {
			continue
		}
		available := limit - used
		if sample.memoryTotal == 0 || limit < sample.memoryTotal {
			sample.memoryTotal = limit
			sample.memoryAvailable = available
		} else if sample.memoryAvailable == 0 || available < sample.memoryAvailable {
			sample.memoryAvailable = available
		}
		return
	}
}

func readHistoryExportUintFile(path string) uint64 {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value := strings.TrimSpace(string(content))
	if value == "" || value == "max" {
		return 0
	}
	result, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return result
}
