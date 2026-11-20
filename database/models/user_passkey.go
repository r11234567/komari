package models

import "time"

// CredentialHash is the SHA-256 of the opaque credential ID. The fixed-size
// binary identity also avoids case-insensitive text indexes on MySQL.
type UserPasskey struct {
	CredentialHash string   `gorm:"type:char(64);primaryKey"`
	CredentialID   string   `gorm:"not null"`
	UserUUID       string   `gorm:"type:varchar(36);not null;index"`
	User           User     `gorm:"foreignKey:UserUUID;references:UUID;constraint:OnDelete:CASCADE,OnUpdate:CASCADE"`
	RPID           string   `gorm:"type:varchar(253);not null"`
	Name           string   `gorm:"type:varchar(80);not null"`
	PublicKey      string   `gorm:"not null"`
	Transports     []string `gorm:"serializer:json"`
	DeviceType     string
	BackedUp       bool
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

type PasskeyImport struct {
	Source    string `gorm:"type:varchar(40);primaryKey"`
	Digest    string `gorm:"type:char(64);not null"`
	RPID      string
	CreatedAt time.Time
}

// PasskeyRevocation records explicitly deleted passkeys so restoring an older
// backup cannot resurrect revoked credentials or permit old exports to re-add them.
type PasskeyRevocation struct {
	CredentialHash string    `gorm:"type:char(64);primaryKey"`
	UserUUID       string    `gorm:"type:varchar(36);not null;index"`
	RPID           string    `gorm:"type:varchar(253);not null"`
	RevokedAt      time.Time `gorm:"not null"`
}
