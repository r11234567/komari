// Package credentialaction is the Go-only consumer boundary for scoped proofs.
// It keeps REST handlers independent of gateway/provider implementations.
package credentialaction

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"

	credential "github.com/komari-monitor/komari/internal/trustedcredential"
)

const Header = "X-Komari-Credential-Authorization"
const AccountEntry = "account:entry"
const LoginSettings = "login-settings:unlock"

type Verifier interface {
	Consume(context.Context, string, credential.Purpose, string) (credential.Authorization, error)
	SessionState(context.Context, string, string) (string, error)
	HasSecondFactor(context.Context, string) (bool, error)
}

type installed struct{ verifier Verifier }

var active atomic.Pointer[installed]

// Install is called once by trusted server initialization; never by JS or RPC.
func Install(v Verifier) {
	if v == nil || !active.CompareAndSwap(nil, &installed{v}) {
		panic("credential action verifier already installed or nil")
	}
}

func Consume(ctx context.Context, token string, purpose credential.Purpose, resource string) (credential.Authorization, error) {
	if v := active.Load(); v != nil {
		return v.verifier.Consume(ctx, token, purpose, resource)
	}
	return credential.Authorization{}, credential.ErrProviderUnavailable
}

func SessionState(ctx context.Context, user, session string) (string, error) {
	if v := active.Load(); v != nil {
		return v.verifier.SessionState(ctx, user, session)
	}
	return "", credential.ErrProviderUnavailable
}

func HasSecondFactor(ctx context.Context, user string) (bool, error) {
	if v := active.Load(); v != nil {
		return v.verifier.HasSecondFactor(ctx, user)
	}
	return true, credential.ErrProviderUnavailable
}

func RESTResource(method, path string) string { return "rest:" + strings.ToUpper(method) + ":" + path }
func PasswordResource(user string) string     { return "account:password:" + user }
func WorkspaceResource(agent, user string) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([]string{agent, user})
	return "workspace:" + strings.TrimSuffix(b.String(), "\n")
}
