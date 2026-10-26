// Package passkeycredential adapts the existing Akizon77 plugin's public-key
// records. It never invokes the plugin's login handler or writes its storage.
package passkeycredential

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/komari-monitor/komari/internal/plugin"
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

// Read returns only the requested user's public credentials. The fingerprint
// omits counters and timestamps: ordinary login activity must not revoke a
// parallel ceremony. Keys, ownership, policy and runtime changes do revoke it.
func Read(ctx context.Context, user string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	var found bool
	var running bool
	var version string
	for _, item := range plugin.List() {
		if item.Short == pluginID {
			found = true
			running = item.Enabled && item.Running
			version = item.Version
			break
		}
	}
	if !found {
		return Snapshot{Revision: "absent"}, nil
	}
	config, err := plugin.GetConfiguration(pluginID)
	if err != nil {
		return Snapshot{}, credential.ErrProviderUnavailable
	}
	root, err := os.OpenRoot(plugin.StorageDir)
	if err != nil {
		return Snapshot{}, credential.ErrProviderUnavailable
	}
	defer root.Close()
	file, err := root.Open(pluginID + "/passkeys.json")
	var records []Record
	if err == nil {
		defer file.Close()
		info, e := file.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() > maxStoreBytes {
			return Snapshot{}, credential.ErrProviderUnavailable
		}
		raw, e := io.ReadAll(io.LimitReader(file, maxStoreBytes+1))
		if e != nil || len(raw) > maxStoreBytes {
			return Snapshot{}, credential.ErrProviderUnavailable
		}
		records, err = decodeStore(raw, user)
		if err != nil {
			return Snapshot{}, err
		}
	} else if !os.IsNotExist(err) {
		return Snapshot{}, credential.ErrProviderUnavailable
	}
	stable := make([]Record, len(records))
	copy(stable, records)
	for i := range stable {
		stable[i].Counter = 0
		stable[i].BackedUp = false
	}
	raw, err := json.Marshal([]any{version, running, plugin.RuntimeGeneration(pluginID), config, stable})
	if err != nil {
		return Snapshot{}, credential.ErrProviderUnavailable
	}
	digest := sha256.Sum256(raw)
	return Snapshot{Revision: hex.EncodeToString(digest[:]), Running: running, Records: records, Config: config}, nil
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
	return context.WithValue(ctx, originKey{}, scheme+"://"+host)
}

func relyingParty(ctx context.Context, config map[string]any) (string, string, bool, error) {
	origin, _ := ctx.Value(originKey{}).(string)
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false, credential.ErrProviderUnavailable
	}
	allowHTTP, _ := config["allow_http_origins"].(bool)
	if u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return "", "", false, credential.ErrProviderUnavailable
	}
	rp, _ := config["rp_id"].(string)
	rp = strings.TrimSpace(rp)
	origins, _ := config["origins"].(string)
	if rp == "" || strings.TrimSpace(origins) == "" {
		rp = u.Hostname()
	} else {
		matched := false
		for _, candidate := range strings.FieldsFunc(origins, func(r rune) bool { return r == ',' || r == '\r' || r == '\n' }) {
			if strings.TrimSpace(candidate) == origin {
				matched = true
			}
		}
		if !matched {
			return "", "", false, credential.ErrProviderUnavailable
		}
	}
	if u.Hostname() != rp && !strings.HasSuffix(u.Hostname(), "."+rp) {
		return "", "", false, credential.ErrProviderUnavailable
	}
	allowSynced, _ := config["allow_synced_credentials"].(bool)
	return rp, origin, allowSynced, nil
}
