package trafficledger

import (
	"github.com/komari-monitor/komari/database/models"
	"time"
)

// Day-only callers retain the existing Beijing-midnight billing contract.
func CurrentTrafficCycle(day *int, now time.Time) (time.Time, string, error) {
	return CurrentTrafficCycleFor(models.Client{TrafficResetDay: day}, now)
}
func CycleContaining(day int, at time.Time) time.Time {
	return scheduleForDay(NormalizedResetDay(&day)).Last(at)
}
func NextCycleStart(start time.Time, day int) time.Time {
	return scheduleForDay(NormalizedResetDay(&day)).Next(start)
}
func trafficCycleInclusiveEnd(start time.Time, day int) time.Time {
	return NextCycleStart(start, day).AddDate(0, 0, -1)
}
func calibrationAppliesToCurrentCycle(day *int, cycle string, now time.Time) bool {
	_, current, err := CurrentTrafficCycle(day, now)
	return err == nil && current == cycle
}
