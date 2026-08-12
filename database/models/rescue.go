package models

import "time"

// ClientRescueHelper stores the last independently reported privileged helper
// state. The ordinary Agent never implies these capabilities when it runs as
// a standard user.
type ClientRescueHelper struct {
	Client             string    `json:"-" gorm:"type:varchar(36);primaryKey"`
	Installed          bool      `json:"installed"`
	GuardianRunning    bool      `json:"guardian_running"`
	HelperRunning      bool      `json:"helper_running"`
	FirewallConfigured bool      `json:"firewall_configured"`
	Version            string    `json:"version" gorm:"type:varchar(100)"`
	HelperInstanceID   string    `json:"helper_instance_id" gorm:"type:varchar(128)"`
	ErrorCode          string    `json:"error_code" gorm:"type:varchar(64)"`
	ErrorMessage       string    `json:"error_message" gorm:"type:varchar(512)"`
	ObservedAt         time.Time `json:"observed_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type RescueSession struct {
	ID               string     `json:"-" gorm:"type:varchar(64);primaryKey"`
	Client           string     `json:"-" gorm:"type:varchar(36);index;not null"`
	Action           int32      `json:"-" gorm:"not null"`
	Arguments        string     `json:"-" gorm:"type:text;not null"`
	State            int32      `json:"-" gorm:"not null"`
	TimeoutSeconds   int64      `json:"-" gorm:"not null"`
	MaxOutputBytes   uint64     `json:"-" gorm:"not null"`
	OutputBytes      uint64     `json:"-" gorm:"not null;default:0"`
	HelperInstanceID string     `json:"-" gorm:"type:varchar(128);index"`
	IdempotencyKey   string     `json:"-" gorm:"type:varchar(128);index"`
	ErrorCode        string     `json:"-" gorm:"type:varchar(64)"`
	ErrorMessage     string     `json:"-" gorm:"type:varchar(512)"`
	CreatedAt        time.Time  `json:"-"`
	StartedAt        *time.Time `json:"-"`
	FinishedAt       *time.Time `json:"-"`
	UpdatedAt        time.Time  `json:"-"`
}

type RescueEvent struct {
	Session      string    `json:"-" gorm:"type:varchar(64);primaryKey"`
	Sequence     uint64    `json:"-" gorm:"primaryKey"`
	OccurredAt   time.Time `json:"-"`
	State        int32     `json:"-" gorm:"not null"`
	Stream       int32     `json:"-" gorm:"not null"`
	Output       []byte    `json:"-" gorm:"type:blob"`
	ErrorCode    string    `json:"-" gorm:"type:varchar(64)"`
	ErrorMessage string    `json:"-" gorm:"type:varchar(512)"`
}
