package messageSender

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/komari-monitor/komari/database"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
	"gorm.io/gorm"
)

// providerConfigured reports whether this channel has a saved connection.
// A missing row is not an error: the old single-channel setting used to
// fall back to sending nothing in that case.
var providerConfigured = func(name string) (bool, error) {
	_, err := database.GetMessageSenderConfigByName(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return false, err
}

// Alert kinds are the rows on the notification settings page. Recovery for a
// kind uses the same channels as the alert itself.
const (
	KindOffline              = "offline"
	KindOnline               = "online"
	KindLoad                 = "load"
	KindTraffic              = "traffic"
	KindExpire               = "expire"
	KindRenew                = "renew"
	KindLogin                = "login"
	KindPingLoss             = "ping_loss"
	KindPingLatency          = "ping_latency"
	KindTrafficReportDaily   = "traffic_report_daily"
	KindTrafficReportWeekly  = "traffic_report_weekly"
	KindTrafficReportMonthly = "traffic_report_monthly"
	KindReturnRoute          = "return_route"
	KindMainlandReachability = "mainland_reachability"
)

// Kinds is the stable order shown in the admin notification settings.
var Kinds = []string{
	KindOffline,
	KindOnline,
	KindLoad,
	KindTraffic,
	KindExpire,
	KindRenew,
	KindLogin,
	KindPingLoss,
	KindPingLatency,
	KindTrafficReportDaily,
	KindTrafficReportWeekly,
	KindTrafficReportMonthly,
	KindReturnRoute,
	KindMainlandReachability,
}

// SeedRoutes copies the previous single channel onto every kind. none and
// empty leave every kind with no channel.
func SeedRoutes(method string) map[string][]string {
	method = strings.TrimSpace(method)
	routes := make(map[string][]string, len(Kinds))
	var channels []string
	if method != "" && method != "none" && method != "empty" {
		channels = []string{method}
	}
	for _, kind := range Kinds {
		copied := append([]string(nil), channels...)
		routes[kind] = copied
	}
	return routes
}

// EnsureRoutes copies the channel selected in channel settings onto every
// alert the first time routes are missing or still empty. A later choice to
// send nothing is kept.
var routeMigrationMu sync.Mutex

func SaveSettings(values map[string]interface{}) error {
	routeMigrationMu.Lock()
	defer routeMigrationMu.Unlock()
	return config.SetMany(values)
}

func EnsureRoutes() error {
	routeMigrationMu.Lock()
	defer routeMigrationMu.Unlock()
	routes, err := config.GetAs[map[string][]string](config.NotificationRoutesKey)
	missing := errors.Is(err, gorm.ErrRecordNotFound)
	if err != nil && !missing {
		return err
	}
	migrated, err := routesMigrated()
	if err != nil {
		return err
	}
	if !migrationNeeded(missing, migrated, routes) {
		if !migrated {
			return config.Set(config.NotificationRoutesMigratedKey, true)
		}
		return nil
	}
	method, err := config.GetAs[string](config.NotificationMethodKey, "none")
	if err != nil {
		return err
	}
	seed, err := methodToSeed(method)
	if err != nil {
		return err
	}
	return config.SetMany(map[string]any{config.NotificationRoutesKey: SeedRoutes(seed), config.NotificationRoutesMigratedKey: true})
}

// routesMigrated reads the one-time flag without writing it. A missing row
// means this install has not migrated yet. A read failure must not be stored
// as false, or the next pass would copy the old channel back onto an empty table.
func routesMigrated() (bool, error) {
	migrated, err := config.GetAs[bool](config.NotificationRoutesMigratedKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return migrated, nil
}

// methodToSeed preserves registered channels and saved legacy plugin selections.
// Unconfigured names seed an empty table.
func methodToSeed(method string) (string, error) {
	method = strings.TrimSpace(method)
	if method == "" || method == "none" || method == "empty" {
		return "", nil
	}
	if NotificationChannelRegistered(method) {
		return method, nil
	}
	// Preserve a previously saved plugin selection even before plugins load at startup.

	configured, err := providerConfigured(method)
	if err != nil || !configured {
		return "", err
	}
	return method, nil
}

// ChannelSelected reports whether any alert currently sends through this channel.
func ChannelSelected(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	routes, err := config.GetAs[map[string][]string](config.NotificationRoutesKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, channels := range routes {
		for _, channel := range channels {
			if channel == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// migrationNeeded reports whether the active channel settings selection still
// has to be copied onto the alert routes.
func migrationNeeded(recordMissing, alreadyMigrated bool, routes map[string][]string) bool {
	if !recordMissing && (alreadyMigrated || routesHaveChannel(routes)) {
		return false
	}
	return true
}

func routesHaveChannel(routes map[string][]string) bool {
	for _, kind := range Kinds {
		if len(routes[kind]) > 0 {
			return true
		}
	}
	return false
}

// NormalizeRoutes checks a settings payload and fills every kind. A kind left
// out of the payload is stored with no channels.
func NormalizeRoutes(raw any) (map[string][]string, error) {
	parsed, err := parseRoutePayload(raw)
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(Kinds))
	for _, kind := range Kinds {
		known[kind] = struct{}{}
	}
	for kind := range parsed {
		if _, ok := known[kind]; !ok {
			return nil, fmt.Errorf("unknown notification kind %s", kind)
		}
	}
	out := make(map[string][]string, len(Kinds))
	for _, kind := range Kinds {
		channels, err := normalizeChannels(parsed[kind])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kind, err)
		}
		out[kind] = channels
	}
	return out, nil
}

func parseRoutePayload(raw any) (map[string][]string, error) {
	switch typed := raw.(type) {
	case map[string][]string:
		return typed, nil
	case map[string]any:
		out := make(map[string][]string, len(typed))
		for kind, value := range typed {
			channels, err := stringList(value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", kind, err)
			}
			out[kind] = channels
		}
		return out, nil
	default:
		return nil, errors.New("notification routes must be an object")
	}
}

func stringList(value any) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return typed, nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			name, ok := item.(string)
			if !ok {
				return nil, errors.New("channel name must be a string")
			}
			out = append(out, name)
		}
		return out, nil
	default:
		return nil, errors.New("channels must be a list")
	}
}

func normalizeChannels(channels []string) ([]string, error) {
	if len(channels) == 0 {
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(channels))
	out := make([]string, 0, len(channels))
	for _, name := range channels {
		name = strings.TrimSpace(name)
		if name == "" || name == "none" || name == "empty" {
			return nil, fmt.Errorf("channel %q cannot be selected", name)
		}
		if _, ok := factory.GetConstructor(name); !ok && !NotificationChannelRegistered(name) {
			// An unloaded plugin's existing route can be kept, but cannot be newly selected.
			selected, err := ChannelSelected(name)
			if err != nil {
				return nil, err
			}
			if !selected {
				return nil, fmt.Errorf("unknown channel %s", name)
			}
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

func storedChannels(kind string) ([]string, error) {
	routes, err := config.GetAs[map[string][]string](config.NotificationRoutesKey)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := EnsureRoutes(); err != nil {
			return nil, err
		}
		routes, err = config.GetAs[map[string][]string](config.NotificationRoutesKey)
	}
	if err != nil {
		return nil, err
	}
	channels := routes[kind]
	if channels == nil {
		return []string{}, nil
	}
	return channels, nil
}

// KindForEvent keeps known legacy API event names compatible with route selection.
func KindForEvent(event string) string {
	switch event {
	case "Offline":
		return KindOffline
	case "Online":
		return KindOnline
	case "Alert":
		return KindLoad
	case "Traffic":
		return KindTraffic
	case "Expire":
		return KindExpire
	case "Renew":
		return KindRenew
	case "Login":
		return KindLogin
	case "DReport":
		return KindTrafficReportDaily
	case "WReport":
		return KindTrafficReportWeekly
	case "MReport":
		return KindTrafficReportMonthly
	case "延迟监测告警":
		return KindPingLoss
	case "回程线路告警":
		return KindReturnRoute
	}
	return ""
}
