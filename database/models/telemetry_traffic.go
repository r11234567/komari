package models

import "time"

// TelemetryTrafficStream is a contiguous consumed outbox watermark. It is
// committed in the same metadata transaction as counters and ledger deltas.
type TelemetryTrafficStream struct {
	StoreID string `gorm:"type:varchar(32);primaryKey"`
	Through int64  `gorm:"not null"`
}

// TelemetryTrafficState retains integer counters independently of metric raw
// retention. FirstObserved records the earliest known coverage of this state.
type TelemetryTrafficState struct {
	Client              string `gorm:"type:varchar(36);primaryKey"`
	FirstObserved       int64  `gorm:"not null"`
	LastObserved        int64  `gorm:"not null"`
	TotalUp             int64  `gorm:"not null"`
	TotalDown           int64  `gorm:"not null"`
	AccountedUp         int64  `gorm:"not null"`
	AccountedDown       int64  `gorm:"not null"`
	Dedicated           bool   `gorm:"not null;default:false"`
	LateReports         int64  `gorm:"not null"`
	HourlyPrunedThrough int64  `gorm:"not null;default:0"`
	MinuteSince         int64  `gorm:"not null;default:0"`
	MinutePrunedThrough int64  `gorm:"not null;default:0"`
	TailPrunedThrough   int64  `gorm:"not null;default:0"`
}

// TelemetryTrafficMinute owns accepted event deltas, not averages of network
// rates. It can be composed into any whole-minute reporting interval.
type TelemetryTrafficMinute struct {
	Client        string `gorm:"type:varchar(36);primaryKey"`
	Start         int64  `gorm:"primaryKey;autoIncrement:false;index"`
	FirstObserved int64  `gorm:"not null"`
	LastObserved  int64  `gorm:"not null"`
	UpBytes       int64  `gorm:"not null"`
	DownBytes     int64  `gorm:"not null"`
}

// TelemetryTrafficTail retains only the short mutable edge needed to answer a
// fixed endpoint inside a minute. Its lifetime is independent of metric raw.
type TelemetryTrafficTail struct {
	Client    string `gorm:"type:varchar(36);primaryKey"`
	Observed  int64  `gorm:"primaryKey;autoIncrement:false;index"`
	UpBytes   int64  `gorm:"not null"`
	DownBytes int64  `gorm:"not null"`
}

// TelemetryTrafficSlice is an hourly ledger split at the configured billing
// reset instant. It preserves exact calendar/cycle boundaries without keeping
// one business row per raw metric sample.
type TelemetryTrafficSlice struct {
	Client        string `gorm:"type:varchar(36);primaryKey"`
	Start         int64  `gorm:"primaryKey;autoIncrement:false"`
	End           int64  `gorm:"not null;index"`
	FirstObserved int64  `gorm:"not null"`
	LastObserved  int64  `gorm:"not null"`
	UpBytes       int64  `gorm:"not null"`
	DownBytes     int64  `gorm:"not null"`
}

type TelemetryTrafficCycle struct {
	Client    string `gorm:"type:varchar(36);primaryKey"`
	Cycle     string `gorm:"type:varchar(64);primaryKey"`
	Start     int64  `gorm:"not null"`
	End       int64  `gorm:"not null"`
	UpBytes   int64  `gorm:"not null"`
	DownBytes int64  `gorm:"not null"`
	Through   int64  `gorm:"not null"`
}

// TelemetryTrafficGap records known volume that could not be assigned, as well
// as holes with no measured volume. It does not contribute to billed totals.
type TelemetryTrafficGap struct {
	Client    string `gorm:"type:varchar(36);primaryKey" json:"-"`
	End       int64  `gorm:"primaryKey;autoIncrement:false;index" json:"end_ns"`
	Start     int64  `gorm:"not null" json:"start_ns"`
	Kind      string `gorm:"type:varchar(24);not null" json:"kind"`
	UpBytes   int64  `gorm:"not null" json:"up_bytes"`
	DownBytes int64  `gorm:"not null" json:"down_bytes"`
}

type TrafficPolicyVersion struct {
	Client    string `gorm:"type:varchar(36);primaryKey"`
	Effective int64  `gorm:"primaryKey;autoIncrement:false"`
	Day       int    `gorm:"not null"`
	Clock     string `gorm:"type:varchar(8);not null"`
	Timezone  string `gorm:"type:varchar(64);not null"`
}

type TrafficCalibrationCutoff struct {
	ID           string `gorm:"type:varchar(32);primaryKey"`
	Client       string `gorm:"type:varchar(36);not null;index:idx_traffic_cutoff,priority:1"`
	Cycle        string `gorm:"type:varchar(64);not null;index:idx_traffic_cutoff,priority:2"`
	Cutoff       int64  `gorm:"not null;index:idx_traffic_cutoff,priority:3"`
	RawUp        int64  `gorm:"not null"`
	RawDown      int64  `gorm:"not null"`
	BasisUp      int64  `gorm:"not null"`
	BasisDown    int64  `gorm:"not null"`
	TargetUp     int64  `gorm:"not null"`
	TargetDown   int64  `gorm:"not null"`
	TargetTotal  *int64
	CountingType string    `gorm:"type:varchar(8);not null"`
	Operator     string    `gorm:"type:varchar(36);not null"`
	Note         string    `gorm:"type:varchar(1200);not null"`
	CreatedAt    time.Time `gorm:"not null"`
}
