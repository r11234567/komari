package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

// Client represents a registered client device
type Client struct {
	UUID                   string     `json:"uuid,omitempty" gorm:"type:varchar(36);primaryKey"`
	Token                  string     `json:"token,omitempty" gorm:"type:varchar(255);unique;not null"`
	PreviousToken          string     `json:"-" gorm:"type:varchar(255);index"`
	PreviousTokenExpiresAt *time.Time `json:"-" gorm:"type:timestamp"`
	Name                   string     `json:"name" gorm:"type:varchar(100)"`
	CpuName                string     `json:"cpu_name" gorm:"type:varchar(100)"`
	Virtualization         string     `json:"virtualization" gorm:"type:varchar(50)"`
	Arch                   string     `json:"arch" gorm:"type:varchar(50)"`
	CpuCores               int        `json:"cpu_cores" gorm:"type:int"`
	CpuPhysicalCores       int        `json:"cpu_physical_cores" gorm:"type:int"`
	OS                     string     `json:"os" gorm:"type:varchar(100)"`
	KernelVersion          string     `json:"kernel_version" gorm:"type:varchar(100)"`
	GpuName                string     `json:"gpu_name" gorm:"type:varchar(100)"`
	IPv4                   string     `json:"ipv4,omitempty" gorm:"type:varchar(100)"`
	IPv6                   string     `json:"ipv6,omitempty" gorm:"type:varchar(100)"`
	Region                 string     `json:"region" gorm:"type:varchar(100)"`
	RegionOverride         string     `json:"region_override" gorm:"type:varchar(16);not null;default:''"`
	Remark                 string     `json:"remark,omitempty" gorm:"type:longtext"`
	PublicRemark           string     `json:"public_remark,omitempty" gorm:"type:longtext"`
	MemTotal               int64      `json:"mem_total" gorm:"type:bigint"`
	SwapTotal              int64      `json:"swap_total" gorm:"type:bigint"`
	DiskTotal              int64      `json:"disk_total" gorm:"type:bigint"`
	Version                string     `json:"version,omitempty" gorm:"type:varchar(100)"`
	Weight                 int        `json:"weight" gorm:"type:int"`
	Price                  float64    `json:"price"`
	BillingCycle           int        `json:"billing_cycle"`
	AutoRenewal            bool       `json:"auto_renewal" gorm:"default:false"` // 是否自动续费
	Currency               string     `json:"currency" gorm:"type:varchar(20);default:'$'"`
	ExpiredAt              *time.Time `json:"expired_at" gorm:"type:timestamp"`
	Group                  string     `json:"group" gorm:"type:varchar(100)"`
	Tags                   string     `json:"tags" gorm:"type:text"` // split by ';'
	Hidden                 bool       `json:"hidden" gorm:"default:false"`
	RemoteControlProtected bool       `json:"remote_control_protected" gorm:"default:false"`
	TrafficLimit           int64      `json:"traffic_limit" gorm:"type:bigint"`
	TrafficLimitType       string     `json:"traffic_limit_type" gorm:"type:varchar(10);default:'max'"` // 流量阈值类型：sum max min up down
	TrafficResetDay        *int       `json:"traffic_reset_day,omitempty" gorm:"type:int"`              // nil: follow agent; 0: disabled; 1-31: monthly reset day
	TrafficResetAllowance  int64      `json:"traffic_reset_allowance" gorm:"type:bigint;not null;default:0"`
	TrafficResetCycle      string     `json:"traffic_reset_cycle,omitempty" gorm:"type:varchar(10);not null;default:''"`
	EffectiveTrafficLimit  int64      `json:"effective_traffic_limit" gorm:"-"`
	EffectiveTrafficType   string     `json:"effective_traffic_type" gorm:"-"`
	DeploymentStatus       string     `json:"deployment_status" gorm:"-"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

// ClientDeploymentProfile stores private per-node installation preferences.
// Config is only exposed through the dedicated administrator API.
type ClientDeploymentProfile struct {
	Client            string     `json:"-" gorm:"type:varchar(36);primaryKey"`
	Config            string     `json:"-" gorm:"type:text;not null"`
	PreviousRuntime   string     `json:"-" gorm:"type:text;not null;default:''"`
	Revision          uint64     `json:"-" gorm:"not null;default:0"`
	AppliedRevision   uint64     `json:"-" gorm:"not null;default:0"`
	DeliveryStatus    string     `json:"-" gorm:"type:varchar(16);not null;default:''"`
	DeliveryError     string     `json:"-" gorm:"type:varchar(512);not null;default:''"`
	SavedAt           *time.Time `json:"-"`
	DeliveryUpdatedAt *time.Time `json:"-"`
	SentAt            *time.Time `json:"-"`
	FinishedAt        *time.Time `json:"-"`
	CreatedAt         time.Time  `json:"-"`
	UpdatedAt         time.Time  `json:"-"`
}

// ClientPrivilegedRevision stores one privileged configuration revision and
// the state of the human decision attached to it.
//
// It is a separate table from ClientDeploymentProfile on purpose. The ordinary
// profile is a desired state an Agent converges on unattended; these settings
// widen what the Agent is allowed to do, so their lifecycle is a sequence of
// human approvals rather than a convergence loop, and mixing the two would
// invite a code path that treats one like the other.
type ClientPrivilegedRevision struct {
	Client   string `json:"-" gorm:"type:varchar(36);primaryKey"`
	Revision uint64 `json:"-" gorm:"primaryKey"`
	// Config is the serialized PrivilegedConfig for this revision.
	Config string `json:"-" gorm:"type:text;not null"`
	// UpgradeClass and Reasons record how the panel classified this revision
	// when it was saved, so the classification cannot drift after the fact.
	UpgradeClass      int32  `json:"-" gorm:"not null;default:0"`
	Reasons           string `json:"-" gorm:"type:text;not null;default:''"`
	FromPrivilegeMode int32  `json:"-" gorm:"not null;default:0"`
	ToPrivilegeMode   int32  `json:"-" gorm:"not null;default:0"`
	State             int32  `json:"-" gorm:"not null;default:0"`
	// TaskID and Nonce belong to a manual upgrade. The nonce is single-use and
	// is what proves an upgrade actually ran on the host rather than being
	// claimed from somewhere else.
	TaskID       string     `json:"-" gorm:"type:varchar(64);index"`
	Nonce        string     `json:"-" gorm:"type:varchar(128);index"`
	NonceExpires *time.Time `json:"-"`
	NonceUsedAt  *time.Time `json:"-"`
	// Operator and LocalAuthentication describe who completed the upgrade and
	// how they were verified on the host. The credential itself never reaches
	// the panel.
	Operator            string     `json:"-" gorm:"type:varchar(128)"`
	LocalAuthentication string     `json:"-" gorm:"type:varchar(64)"`
	ActivePrivilegeMode int32      `json:"-" gorm:"not null;default:0"`
	ErrorDetail         string     `json:"-" gorm:"type:varchar(512);not null;default:''"`
	PreviousRevision    uint64     `json:"-" gorm:"not null;default:0"`
	SavedAt             time.Time  `json:"-"`
	ConfirmedAt         *time.Time `json:"-"`
	FinishedAt          *time.Time `json:"-"`
	CreatedAt           time.Time  `json:"-"`
	UpdatedAt           time.Time  `json:"-"`
}

// AgentEnrollment is one device authorization attempt.
//
// Rows are short-lived: an attempt that nobody approves expires, and the
// device code stops being usable. Keeping them in their own table rather than
// on the client record means an unapproved attempt never creates a half-real
// machine in the panel.
type AgentEnrollment struct {
	DeviceCode string `json:"-" gorm:"type:varchar(128);primaryKey"`
	// UserCode is what a human types or reads back. It is indexed because the
	// approval page looks an attempt up by it.
	UserCode string `json:"-" gorm:"type:varchar(32);uniqueIndex;not null"`
	State    int32  `json:"-" gorm:"not null;default:0"`
	// AgentPublicKey binds the credentials this attempt will issue to a key
	// the requesting host generated and never transmitted in private form.
	AgentPublicKey  string `json:"-" gorm:"type:text;not null"`
	AgentKeyID      string `json:"-" gorm:"type:varchar(64)"`
	KeyAlgorithm    int32  `json:"-" gorm:"not null;default:0"`
	Hostname        string `json:"-" gorm:"type:varchar(255)"`
	OperatingSystem string `json:"-" gorm:"type:varchar(64)"`
	Architecture    string `json:"-" gorm:"type:varchar(32)"`
	AgentVersion    string `json:"-" gorm:"type:varchar(64)"`
	HostFingerprint string `json:"-" gorm:"type:varchar(128);index"`
	RequestedScopes string `json:"-" gorm:"type:text;not null;default:''"`
	RemoteIP        string `json:"-" gorm:"type:varchar(64)"`
	// Client is set once an approval binds this attempt to a machine, either a
	// newly created one or an existing one being re-enrolled.
	Client     string     `json:"-" gorm:"type:varchar(36);index"`
	ApprovedBy string     `json:"-" gorm:"type:varchar(36)"`
	ApprovedAt *time.Time `json:"-"`
	ExpiresAt  time.Time  `json:"-"`
	// LastPolledAt supports slow-down responses without a separate counter.
	LastPolledAt *time.Time `json:"-"`
	CreatedAt    time.Time  `json:"-"`
	UpdatedAt    time.Time  `json:"-"`
}

// AgentCredential is one issued access/refresh pair.
//
// The refresh token is stored hashed. A panel database that leaks must not
// hand over working credentials for every machine, and the server only ever
// needs to check a presented token rather than reproduce one.
type AgentCredential struct {
	Client string `json:"-" gorm:"type:varchar(36);primaryKey"`
	// AccessTokenHash and RefreshTokenHash are hex SHA-256 digests.
	AccessTokenHash  string `json:"-" gorm:"type:varchar(64);index"`
	RefreshTokenHash string `json:"-" gorm:"type:varchar(64);index"`
	// PreviousRefreshHash keeps the superseded token usable for a short grace
	// period, so an Agent that crashes between receiving a rotation and
	// persisting it is not locked out.
	PreviousRefreshHash    string     `json:"-" gorm:"type:varchar(64);index"`
	PreviousRefreshExpires *time.Time `json:"-"`
	AccessExpiresAt        time.Time  `json:"-"`
	RefreshExpiresAt       time.Time  `json:"-"`
	// AgentPublicKey is what refresh proofs are verified against, which is
	// what stops a leaked refresh token from being usable on its own.
	AgentPublicKey string     `json:"-" gorm:"type:text;not null"`
	AgentKeyID     string     `json:"-" gorm:"type:varchar(64)"`
	KeyAlgorithm   int32      `json:"-" gorm:"not null;default:0"`
	Scopes         string     `json:"-" gorm:"type:text;not null;default:''"`
	RevokedAt      *time.Time `json:"-"`
	RevokedReason  string     `json:"-" gorm:"type:varchar(255)"`
	CreatedAt      time.Time  `json:"-"`
	UpdatedAt      time.Time  `json:"-"`
}

// ControlPlaneKey is a signing key the panel uses for signed instructions.
//
// Only the public half is stored here; the private half lives in the secure
// config store. Agents pin these on first contact, so rotation adds a row
// rather than replacing one, letting old and new coexist during a migration.
type ControlPlaneKey struct {
	KeyID     string     `json:"-" gorm:"type:varchar(64);primaryKey"`
	Algorithm int32      `json:"-" gorm:"not null;default:0"`
	PublicKey string     `json:"-" gorm:"type:text;not null"`
	NotBefore time.Time  `json:"-"`
	NotAfter  *time.Time `json:"-"`
	CreatedAt time.Time  `json:"-"`
	UpdatedAt time.Time  `json:"-"`
}

// User represents an authenticated user
type User struct {
	UUID      string    `json:"uuid,omitempty" gorm:"type:varchar(36);primaryKey"`
	Username  string    `json:"username" gorm:"type:varchar(50);unique;not null"`
	Passwd    string    `json:"passwd,omitempty" gorm:"type:varchar(255);not null"` // Hashed password
	SSOType   string    `json:"sso_type" gorm:"type:varchar(20)"`                   // e.g., "github", "google"
	SSOID     string    `json:"sso_id" gorm:"type:varchar(100)"`                    // OAuth provider's user ID
	TwoFactor string    `json:"two_factor,omitempty" gorm:"type:varchar(255)"`      // 2FA secret
	Language  string    `json:"language,omitempty" gorm:"type:varchar(32);not null;default:''"`
	Color     string    `json:"color,omitempty" gorm:"type:varchar(16);not null;default:''"`
	Sessions  []Session `json:"sessions,omitempty" gorm:"foreignKey:UUID;references:UUID;constraint:OnDelete:CASCADE,OnUpdate:CASCADE"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TwoFactorCounter records TOTP time steps that have already been accepted, so
// a code cannot be used twice. The composite primary key is the enforcement:
// a second insert for the same step is rejected by the database rather than by
// a read-then-write that two concurrent logins could both pass.
type TwoFactorCounter struct {
	UUID      string    `json:"uuid" gorm:"type:varchar(36);primaryKey"`
	Counter   int64     `json:"counter" gorm:"primaryKey"`
	UsedAt    time.Time `json:"used_at" gorm:"type:timestamp"`
	ExpiresAt time.Time `json:"expires_at" gorm:"index"`
}

// Session manages user sessions
type Session struct {
	UUID            string    `json:"uuid" gorm:"type:varchar(36)"`
	Session         string    `json:"session" gorm:"type:varchar(255);primaryKey;uniqueIndex:idx_sessions_session;not null"`
	UserAgent       string    `json:"user_agent" gorm:"type:text"`
	Ip              string    `json:"ip" gorm:"type:varchar(100)"`
	LoginMethod     string    `json:"login_method" gorm:"type:varchar(50)"`
	LatestOnline    time.Time `json:"latest_online" gorm:"type:timestamp"`
	LatestUserAgent string    `json:"latest_user_agent" gorm:"type:text"`
	LatestIp        string    `json:"latest_ip" gorm:"type:varchar(100)"`
	Expires         time.Time `json:"expires" gorm:"not null"`
	CreatedAt       time.Time `json:"created_at"`
}

// Record logs client metrics over time
type Record struct {
	Client         string    `json:"client" gorm:"type:varchar(36);index"`
	Time           time.Time `json:"time" gorm:"index"`
	Cpu            float32   `json:"cpu" gorm:"type:decimal(5,2)"` // e.g., 75.50%
	Gpu            float32   `json:"gpu" gorm:"type:decimal(5,2)"`
	Ram            int64     `json:"ram" gorm:"type:bigint"`
	RamTotal       int64     `json:"ram_total" gorm:"type:bigint"`
	Swap           int64     `json:"swap" gorm:"type:bigint"`
	SwapTotal      int64     `json:"swap_total" gorm:"type:bigint"`
	Load           float32   `json:"load" gorm:"type:decimal(5,2)"`
	Temp           float32   `json:"temp" gorm:"type:decimal(5,2)"`
	Disk           int64     `json:"disk" gorm:"type:bigint"`
	DiskTotal      int64     `json:"disk_total" gorm:"type:bigint"`
	NetIn          int64     `json:"net_in" gorm:"type:bigint"`
	NetOut         int64     `json:"net_out" gorm:"type:bigint"`
	NetTotalUp     int64     `json:"net_total_up" gorm:"type:bigint"`
	NetTotalDown   int64     `json:"net_total_down" gorm:"type:bigint"`
	TrafficUp      int64     `json:"traffic_up" gorm:"type:bigint"`
	TrafficDown    int64     `json:"traffic_down" gorm:"type:bigint"`
	TrafficUpSet   bool      `json:"-" gorm:"-"`
	TrafficDownSet bool      `json:"-" gorm:"-"`
	Process        int       `json:"process"`
	Connections    int       `json:"connections"`
	ConnectionsUdp int       `json:"connections_udp"`
	//Uptime         int64     `json:"uptime" gorm:"type:bigint"`
}

// GPURecord logs individual GPU metrics over time
type GPURecord struct {
	Client      string    `json:"client" gorm:"type:varchar(36);index"` // 客户端UUID
	Time        time.Time `json:"time" gorm:"index"`                    // 记录时间
	DeviceIndex int       `json:"device_index" gorm:"index"`            // GPU设备索引 (0,1,2...)
	DeviceName  string    `json:"device_name" gorm:"type:varchar(100)"` // GPU型号
	MemTotal    int64     `json:"mem_total" gorm:"type:bigint"`         // 显存总量(字节)
	MemUsed     int64     `json:"mem_used" gorm:"type:bigint"`          // 显存使用(字节)
	Utilization float32   `json:"utilization" gorm:"type:decimal(5,2)"` // GPU使用率(%)
	Temperature int       `json:"temperature"`                          // GPU温度(°C)
}

// StringArray represents a slice of strings stored as JSON in the database
// StringArray 存储为 JSON 的字符串切片类型
type StringArray []string

func (sa *StringArray) Scan(value interface{}) error {
	var bytes []byte
	switch v := value.(type) {
	case nil:
		*sa = StringArray{}
		return nil
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		return fmt.Errorf("failed to scan StringArray: unsupported value type %T", value)
	}
	if len(bytes) == 0 {
		*sa = StringArray{}
		return nil
	}
	return json.Unmarshal(bytes, sa)
}

func (sa StringArray) Value() (driver.Value, error) {
	return json.Marshal(sa)
}
