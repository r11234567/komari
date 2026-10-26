// Package notificationotp implements native, addressed notification OTP proofs.
// It uses configured notification transports without the alert event pipeline.
package notificationotp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"math/big"
	"strings"
	"sync"
	"time"

	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/utils/messageSender"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

const (
	SendCooldown = time.Minute
	SendWindow   = time.Hour
	// A workspace can legitimately relock every three idle minutes. Sixty
	// messages/hour leaves room for login, account and sensitive action proofs.
	MaxSendsPerWindow = 60
	MaxSendBuckets    = 4096
	CodeLength        = 8
	codeAlphabet      = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

type Destination struct{ Kind, Address, Transport string }
type DeliverFunc func(context.Context, string, factory.CredentialDelivery) error
type Options struct {
	Now     func() time.Time
	Deliver DeliverFunc
}
type sendBudget struct {
	Count         int
	Expires, Next time.Time
	Challenge     string
}
type Engine struct {
	mu      sync.Mutex
	now     func() time.Time
	deliver DeliverFunc
	budgets map[[32]byte]sendBudget
	key     []byte
}

func New(options Options) *Engine {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Deliver == nil {
		options.Deliver = messageSender.SendCredential
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("native OTP entropy unavailable")
	}
	return &Engine{now: options.Now, deliver: options.Deliver, budgets: map[[32]byte]sendBudget{}, key: key}
}

// Prepare must receive a server-resolved verified destination, or a new contact
// whose binding was separately authorized by an existing trusted credential.
// A caller-supplied address alone must never authorize a security operation.
func (e *Engine) Prepare(ctx context.Context, a credential.Attempt, d Destination) (credential.Prompt, credential.VerifyFunc, error) {
	if ctx == nil || ctx.Err() != nil || a.Binding.UserID == "" || a.ChallengeID == "" || !e.now().Before(a.ExpiresAt) {
		return credential.Prompt{}, nil, credential.ErrUnauthenticated
	}
	address, err := factory.NormalizeCredentialAddress(d.Kind, d.Address)
	if err != nil || d.Transport == "" {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	d.Address = address
	if err := e.reserve(a.Binding.UserID, d, a.ChallengeID); err != nil {
		return credential.Prompt{}, nil, err
	}
	code, err := newCode()
	if err != nil {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	// Store only an HMAC, salted by this challenge/binding/target. The expected
	// plaintext OTP is used once for delivery and is not retained in the verifier.
	prefix, _ := json.Marshal([]string{a.ChallengeID, a.Binding.UserID, a.Binding.SessionDigest, a.Binding.SecurityVersion, string(a.Request.Purpose), a.Request.Resource, d.Kind, d.Address, d.Transport})
	expected := e.digest(prefix, code)
	err = e.deliver(ctx, d.Transport, factory.CredentialDelivery{Kind: d.Kind, Address: d.Address, Title: "Komari verification code", Message: "Your Komari verification code is: " + code + "\nIt expires in 3 minutes. Do not share this code. If you did not request it, ignore this message."})
	if err != nil {
		if ctx.Err() != nil {
			return credential.Prompt{}, nil, ctx.Err()
		}
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	prompt, _ := json.Marshal(map[string]any{"length": CodeLength, "charset": "alphanumeric", "method": "notification_otp", "destination": MaskAddress(d.Kind, d.Address)})
	expires := a.ExpiresAt
	verify := func(ctx context.Context, raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.now().Before(expires) {
			return credential.ErrVerificationFailed
		}
		var proof struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &proof) != nil || len(proof.Code) != CodeLength {
			return credential.ErrVerificationFailed
		}
		for _, c := range proof.Code {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
				return credential.ErrVerificationFailed
			}
		}
		got := e.digest(prefix, strings.ToUpper(proof.Code))
		if subtle.ConstantTimeCompare(expected, got) != 1 {
			return credential.ErrVerificationFailed
		}
		e.releaseCooldown(d, a.ChallengeID)
		return nil
	}
	return credential.Prompt{Type: "otp", Data: prompt}, verify, nil
}

func (e *Engine) digest(prefix []byte, code string) []byte {
	mac := hmac.New(sha256.New, e.key)
	_, _ = mac.Write(prefix)
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(code))
	return mac.Sum(nil)
}

// Rejection sampling guarantees an unbiased code containing both letters and
// digits. Delivery is the only use of the plaintext outside this stack frame.
func newCode() (string, error) {
	bound := big.NewInt(int64(len(codeAlphabet)))
	for {
		var code [CodeLength]byte
		letter, digit := false, false
		for i := range code {
			n, err := rand.Int(rand.Reader, bound)
			if err != nil {
				return "", err
			}
			code[i] = codeAlphabet[n.Int64()]
			letter = letter || code[i] >= 'A' && code[i] <= 'Z'
			digit = digit || code[i] >= '0' && code[i] <= '9'
		}
		if letter && digit {
			return string(code[:]), nil
		}
	}
}

func (e *Engine) targetKey(d Destination) (key [32]byte) {
	target, _ := json.Marshal([]string{d.Kind, d.Address})
	copy(key[:], e.digest([]byte("send-target"), string(target)))
	return
}

func (e *Engine) releaseCooldown(d Destination, challenge string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := e.targetKey(d)
	if b, ok := e.budgets[key]; ok && b.Challenge == challenge {
		b.Next = time.Time{}
		b.Challenge = ""
		e.budgets[key] = b
	}
}

func (e *Engine) reserve(user string, d Destination, challenge string) error {
	var userKey, targetKey [32]byte
	copy(userKey[:], e.digest([]byte("send-user"), user))
	targetKey = e.targetKey(d)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for key, b := range e.budgets {
		if !now.Before(b.Expires) {
			delete(e.budgets, key)
		}
	}
	newKeys := 0
	for _, key := range [][32]byte{userKey, targetKey} {
		b, exists := e.budgets[key]
		if exists && key == targetKey && now.Before(b.Next) {
			return credential.ErrSendCooldown
		}
		if exists && b.Count >= MaxSendsPerWindow {
			return credential.ErrSendWindow
		}
		if !exists {
			newKeys++
		}
	}
	if len(e.budgets)+newKeys > MaxSendBuckets {
		return credential.ErrLimit
	}
	for _, key := range [][32]byte{userKey, targetKey} {
		b, exists := e.budgets[key]
		if !exists {
			b.Expires = now.Add(SendWindow)
		}
		b.Count++
		b.Next = now.Add(SendCooldown)
		b.Challenge = challenge
		e.budgets[key] = b
	}
	return nil
}

func MaskAddress(kind, address string) string {
	if kind == "email" {
		at := strings.LastIndexByte(address, '@')
		if at > 0 {
			return address[:1] + "***" + address[at:]
		}
	}
	if len(address) > 4 {
		return "***" + address[len(address)-4:]
	}
	return "***"
}

// Provider resolves a user's already-verified contact from server storage.
// Native account binding uses the same Engine but a separate authorizing action.
type Provider struct {
	Kind    string
	Engine  *Engine
	Resolve func(context.Context, credential.Binding) (Destination, error)
}

func (p *Provider) Descriptor() credential.Descriptor {
	kind := "email_otp"
	name := "Email OTP"
	if p.Kind == "telegram" {
		kind = "telegram_otp"
		name = "Telegram OTP"
	}
	return credential.Descriptor{ID: "native:" + kind, DisplayName: name, Kind: kind, Purposes: []credential.Purpose{credential.BindContact, credential.ManageCredentials, credential.UnlockWorkspace, credential.LoginSettings, credential.SensitiveOperation, credential.PasswordLogin}}
}
func (p *Provider) Available(ctx context.Context, b credential.Binding) (bool, error) {
	if p.Resolve == nil || p.Engine == nil {
		return false, nil
	}
	d, err := p.Resolve(ctx, b)
	return err == nil && d.Kind == p.Kind && d.Address != "" && d.Transport != "", nil
}
func (p *Provider) Begin(ctx context.Context, a credential.Attempt) (credential.Prompt, credential.VerifyFunc, error) {
	if p.Resolve == nil || p.Engine == nil {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	d, err := p.Resolve(ctx, a.Binding)
	if err != nil || d.Kind != p.Kind {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	return p.Engine.Prepare(ctx, a, d)
}
