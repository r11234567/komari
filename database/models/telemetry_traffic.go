package models

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
