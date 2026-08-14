//go:build !linux

package admin

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
	return historyExportSystemSample{loadRatio: -1}
}
