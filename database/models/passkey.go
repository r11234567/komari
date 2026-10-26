package models

// PasskeyCounter is the trusted adapter's high-water mark. The plugin continues
// owning its JSON records; no private keys or login/session tokens are copied.
type PasskeyCounter struct {
	UserUUID  string `json:"-" gorm:"type:varchar(36);primaryKey"`
	KeyDigest string `json:"-" gorm:"type:varchar(64);primaryKey"`
	Counter   uint32 `json:"-"`
	User      User   `json:"-" gorm:"foreignKey:UserUUID;references:UUID;constraint:OnDelete:CASCADE,OnUpdate:CASCADE"`
}
