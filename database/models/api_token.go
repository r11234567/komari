package models

import "time"

// APIToken stores a verifier, never the bearer secret. Policies are immutable;
// rotation creates a new row and revokes the old row in the same transaction.
type APIToken struct {
	ID           string `gorm:"primaryKey;size:36"`
	OwnerUUID    string `gorm:"not null;index;size:36"`
	Name         string `gorm:"not null;size:80"`
	SecretHash   string `gorm:"not null;size:64" json:"-"`
	Scopes       string `gorm:"not null;type:text"`
	AgentIDs     string `gorm:"not null;type:text"`
	AllAgents    bool
	AllowedCIDRs string `gorm:"not null;type:text"`
	CreatedAt    time.Time
	ExpiresAt    time.Time `gorm:"not null;index"`
	LastUsedAt   *time.Time
	RevokedAt    *time.Time `gorm:"index"`
}
