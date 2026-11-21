// Package credentialaction is the Go-only consumer boundary for scoped proofs.
// It keeps REST handlers independent of gateway/provider implementations.
package credentialaction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
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

// ScopedResource hashes public target identifiers only, not passwords/proofs.
// Fixed-size resources also support large execution target batches.
func ScopedResource(action string, targets ...string) string {
	if targets == nil {
		targets = []string{}
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(targets)
	digest := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return "action:" + action + ":" + hex.EncodeToString(digest[:])
}

func WorkspaceResource(agent, user string) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([]string{agent, user})
	return "workspace:" + strings.TrimSuffix(b.String(), "\n")
}

// ExecutionSpecDigest computes the canonical SHA-256 hex digest of an execution
// specification using a positional JSON tuple identical to komari-web.
func ExecutionSpecDigest(command string, arguments []string, environment map[string]string, workingDirectory string, timeoutSeconds int64, maxOutputBytes uint64, runAsRoot bool) string {
	args := arguments
	if args == nil {
		args = []string{}
	}
	envKeys := make([]string, 0, len(environment))
	for k := range environment {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	envPairs := make([][2]string, 0, len(envKeys))
	for _, k := range envKeys {
		envPairs = append(envPairs, [2]string{k, environment[k]})
	}
	tuple := []any{
		"v1",
		command,
		args,
		envPairs,
		workingDirectory,
		timeoutSeconds,
		maxOutputBytes,
		runAsRoot,
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(tuple)
	digest := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return hex.EncodeToString(digest[:])
}

// ExecutionTargets binds the execution step-up authorization proof to target
// agents, root selection, and the full canonical execution specification.
func ExecutionTargets(agentIDs []string, command string, arguments []string, environment map[string]string, workingDirectory string, timeoutSeconds int64, maxOutputBytes uint64, runAsRoot bool) []string {
	targets := make([]string, 0, len(agentIDs)+2)
	targets = append(targets, agentIDs...)
	if runAsRoot {
		targets = append(targets, "run-as:root")
	}
	targets = append(targets, "spec:"+ExecutionSpecDigest(command, arguments, environment, workingDirectory, timeoutSeconds, maxOutputBytes, runAsRoot))
	return targets
}

// RescueSpecDigest computes the canonical SHA-256 hex digest of a rescue
// session specification using a positional JSON tuple identical to komari-web.
func RescueSpecDigest(arguments []string, timeoutSeconds int64, maxOutputBytes uint64, sshPort uint32) string {
	args := arguments
	if args == nil {
		args = []string{}
	}
	tuple := []any{
		"v1",
		args,
		timeoutSeconds,
		maxOutputBytes,
		sshPort,
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(tuple)
	digest := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return hex.EncodeToString(digest[:])
}

// RescueTargets binds the rescue step-up authorization proof to target agent,
// action, resolved SSH port, and the full canonical rescue specification.
func RescueTargets(agentID string, action int32, arguments []string, timeoutSeconds int64, maxOutputBytes uint64, sshPort uint32) []string {
	return []string{
		agentID,
		strconv.Itoa(int(action)),
		strconv.Itoa(int(sshPort)),
		"spec:" + RescueSpecDigest(arguments, timeoutSeconds, maxOutputBytes, sshPort),
	}
}
