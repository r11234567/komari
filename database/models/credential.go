package models

import "time"

// CredentialContact contains a verified private recipient, not notification
// transport credentials. A user can have one email and one Telegram contact.
type CredentialContact struct {
	UserUUID   string    `json:"-" gorm:"type:varchar(36);primaryKey"`
	Kind       string    `json:"kind" gorm:"type:varchar(16);primaryKey"`
	Address    string    `json:"-" gorm:"type:varchar(320);not null"`
	Transport  string    `json:"transport" gorm:"type:varchar(128);not null"`
	VerifiedAt time.Time `json:"verified_at"`
	User       User      `json:"-" gorm:"foreignKey:UserUUID;references:UUID;constraint:OnDelete:CASCADE,OnUpdate:CASCADE"`
}
