package metricstore

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var maintenancePressureState struct {
	sync.Mutex
	lastAt    time.Time
	lastTotal uint64
	lastSteal uint64
}

// MaintenanceTimeout returns a bounded wall-clock budget for one maintenance
// pass. Linux CPU steal time is used as a pressure signal; when the host gives
// the process less CPU, the pass gets more wall time but never an unbounded
// lease. On systems without /proc/stat the normal budget is retained.
func MaintenanceTimeout(now time.Time) time.Duration {
	steal := maintenanceStealRatio(now)
	base := maintenanceRunBudget
	effective := 1 - steal
	if effective < 0.2 {
		effective = 0.2
	}
	budget := time.Duration(float64(base) / effective)
	if budget < base {
		budget = base
	}
	if budget > 90*time.Second {
		budget = 90 * time.Second
	}
	return budget + 3*time.Second
}

func maintenanceStealRatio(now time.Time) float64 {
	total, steal, ok := readCPUJiffies()
	if !ok {
		return 0
	}
	maintenancePressureState.Lock()
	defer maintenancePressureState.Unlock()
	if maintenancePressureState.lastAt.IsZero() || total <= maintenancePressureState.lastTotal || steal < maintenancePressureState.lastSteal {
		maintenancePressureState.lastAt = now
		maintenancePressureState.lastTotal = total
		maintenancePressureState.lastSteal = steal
		return 0
	}
	dTotal := total - maintenancePressureState.lastTotal
	dSteal := steal - maintenancePressureState.lastSteal
	maintenancePressureState.lastAt = now
	maintenancePressureState.lastTotal = total
	maintenancePressureState.lastSteal = steal
	if dTotal == 0 || dSteal > dTotal {
		return 0
	}
	return float64(dSteal) / float64(dTotal)
}

func readCPUJiffies() (uint64, uint64, bool) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 9 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var values [8]uint64
	for i := range values {
		value, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		values[i] = value
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	return total, values[7], true
}
