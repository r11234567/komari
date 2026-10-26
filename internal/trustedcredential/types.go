// Package trustedcredential owns verification challenges and scoped, single-use
// authorizations. Providers are registered by trusted server code, never by an
// HTTP/RPC success flag or by a plugin declaring itself trusted.
package trustedcredential

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

type Purpose string

const (
	BindContact           Purpose = "bind_contact"
	ManageCredentials     Purpose = "manage_credentials"
	UnlockWorkspace       Purpose = "unlock_workspace"
	LoginSettings         Purpose = "login_settings"
	SensitiveOperation    Purpose = "sensitive_operation"
	ChallengeLifetime             = 3 * time.Minute
	AuthorizationLifetime         = time.Minute
	MaxAttempts                   = 5
	MaxUserAttempts               = 10
	AttemptWindow                 = 15 * time.Minute
	MaxAttemptBuckets             = 1024
	MaxChallenges                 = 256
	MaxAuthorizations             = 256
	MaxPerSession                 = 4
	MaxPerUser                    = 8
	MaxParametersBytes            = 16 << 10
	MaxProofBytes                 = 64 << 10
	MaxPromptBytes                = 64 << 10
)

var (
	ErrUnauthenticated     = errors.New("a live interactive user session is required")
	ErrInvalidArgument     = errors.New("invalid credential verification parameters")
	ErrProviderUnavailable = errors.New("credential provider is unavailable")
	ErrVerificationFailed  = errors.New("credential verification failed")
	ErrNotFound            = errors.New("verification or authorization is no longer available")
	ErrBusy                = errors.New("verification is already in progress")
	ErrLimit               = errors.New("credential verification capacity reached")
)

// Binding contains digests only. The gateway derives it from a live session;
// clients never choose a user, session digest or security version.
type Binding struct {
	UserID          string
	SessionDigest   string
	SecurityVersion string
}

type Descriptor struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Kind        string    `json:"kind"`
	Purposes    []Purpose `json:"purposes"`
}

type Request struct {
	ProviderID string          `json:"provider_id"`
	Purpose    Purpose         `json:"purpose"`
	Resource   string          `json:"resource"`
	Parameters json.RawMessage `json:"parameters"`
}

type Attempt struct {
	ChallengeID string
	Binding     Binding
	Request     Request
	ExpiresAt   time.Time
}

type Prompt struct {
	Type string          `json:"prompt_type"`
	Data json.RawMessage `json:"prompt"`
}

type Challenge struct {
	ID        string    `json:"challenge_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Prompt
}

type Authorization struct {
	Token      string    `json:"grant_token"`
	ProviderID string    `json:"provider_id"`
	Purpose    Purpose   `json:"purpose"`
	Resource   string    `json:"resource"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// VerifyFunc must honor its context and must not issue a session or grant.
// Provider-private verifier state never appears in a public prompt.
type VerifyFunc func(context.Context, json.RawMessage) error

type Provider interface {
	Descriptor() Descriptor
	Available(context.Context, Binding) (bool, error)
	Begin(context.Context, Attempt) (Prompt, VerifyFunc, error)
}

type Options struct {
	Now             func() time.Time
	Random          io.Reader
	ValidateBinding func(context.Context, Binding) error
}

func validPurpose(p Purpose) bool {
	switch p {
	case BindContact, ManageCredentials, UnlockWorkspace, LoginSettings, SensitiveOperation:
		return true
	}
	return false
}
