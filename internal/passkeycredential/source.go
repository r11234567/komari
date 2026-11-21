// Package passkeycredential provides the native WebAuthn / Passkey credential
// provider and v1 record migration helpers. The legacy komari-passkey plugin
// runtime is retired and never consulted at runtime.
package passkeycredential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	credential "github.com/komari-monitor/komari/internal/trustedcredential"
)

const pluginID = "komari-passkey"
const maxStoreBytes = 1 << 20

type Record struct {
	UserUUID   string   `json:"userUUID"`
	ID         string   `json:"id"`
	PublicKey  string   `json:"publicKey"`
	Counter    uint32   `json:"counter"`
	Transports []string `json:"transports"`
	DeviceType string   `json:"deviceType"`
	BackedUp   bool     `json:"backedUp"`
}

type Snapshot struct {
	Revision string
	Running  bool
	Records  []Record
	Config   map[string]any
}

// Read returns only the requested user's native public credentials. The
// fingerprint omits counters and timestamps: ordinary login activity must not
// revoke a parallel ceremony. Keys, ownership, policy and runtime changes do.
func Read(ctx context.Context, user string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return nativeSnapshot(ctx, user)
}

func decodeStore(raw []byte, user string) ([]Record, error) {
	var store struct {
		Version     int      `json:"version"`
		Credentials []Record `json:"credentials"`
	}
	if json.Unmarshal(raw, &store) != nil || store.Version != 1 || len(store.Credentials) > 1024 {
		return nil, credential.ErrProviderUnavailable
	}
	result := []Record{}
	seen := map[string]bool{}
	for _, record := range store.Credentials {
		if record.UserUUID != user {
			continue
		}
		id, err := base64.RawURLEncoding.Strict().DecodeString(record.ID)
		key, keyErr := base64.RawURLEncoding.Strict().DecodeString(record.PublicKey)
		if err != nil || keyErr != nil || len(id) == 0 || len(id) > 1024 || len(key) == 0 || len(key) > 4096 || seen[record.ID] || len(record.Transports) > 8 || (record.DeviceType != "singleDevice" && record.DeviceType != "multiDevice") {
			return nil, credential.ErrProviderUnavailable
		}
		seen[record.ID] = true
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

type originKey struct{}

// WithRequestOrigin receives the transport's actual server host and secure
// scheme, never a client-selected verification parameter or forwarded host.
func WithRequestOrigin(ctx context.Context, host string, secure bool) context.Context {
	scheme := "http"
	if secure {
		scheme = "https"
	}
	return context.WithValue(ctx, originKey{}, scheme+"://"+strings.ToLower(strings.TrimSpace(host)))
}

func relyingParty(ctx context.Context, config map[string]any) (string, string, bool, error) {
	origin, _ := ctx.Value(originKey{}).(string)
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false, credential.ErrProviderUnavailable
	}
	origin = strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	hostname := strings.ToLower(u.Hostname())
	allowHTTP, _ := config["allow_http_origins"].(bool)
	if !strings.EqualFold(u.Scheme, "https") && !(strings.EqualFold(u.Scheme, "http") && allowHTTP && (hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1")) {
		return "", "", false, credential.ErrProviderUnavailable
	}
	rp, _ := config["rp_id"].(string)
	rp = strings.ToLower(strings.TrimSpace(rp))
	origins, _ := config["origins"].(string)
	if rp == "" || strings.TrimSpace(origins) == "" {
		rp = hostname
	} else {
		matched := false
		for _, candidate := range strings.FieldsFunc(origins, func(r rune) bool { return r == ',' || r == '\r' || r == '\n' }) {
			if strings.EqualFold(strings.TrimSpace(candidate), origin) {
				matched = true
			}
		}
		if !matched {
			return "", "", false, credential.ErrProviderUnavailable
		}
	}
	if hostname != rp && !strings.HasSuffix(hostname, "."+rp) {
		return "", "", false, credential.ErrProviderUnavailable
	}
	allowSynced, _ := config["allow_synced_credentials"].(bool)
	return rp, origin, allowSynced, nil
}
