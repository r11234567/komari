// Package plugingateway is the shared application boundary for plugin management.
// Internal RPC retains its existing names and result shapes; Connect uses the
// same operations without opening admin namespaces on the RPC2 transport.
package plugingateway

import (
	"context"
	"errors"
	"strings"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/pkg/rpc"
)

var ErrPermissionDenied = errors.New("plugin management requires an admin principal")
var ErrShortRequired = errors.New("plugin short name is required")
var ErrManifestLookup = errors.New("plugin manifest lookup failed")

// Preserve the original manifest error message and transport NotFound mapping.
type manifestLookupError struct{ cause error }

func (e manifestLookupError) Error() string        { return e.cause.Error() }
func (e manifestLookupError) Unwrap() error        { return e.cause }
func (e manifestLookupError) Is(target error) bool { return target == ErrManifestLookup }

func authorize(ctx context.Context, short string, required bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil || !meta.Principal.HasRole(rpc.RoleAdmin) {
		return "", ErrPermissionDenied
	}
	short = strings.TrimSpace(short)
	if required && short == "" {
		return "", ErrShortRequired
	}
	return short, nil
}

func List(ctx context.Context) ([]plugin.Info, error) {
	if _, err := authorize(ctx, "", false); err != nil {
		return nil, err
	}
	return plugin.List(), nil
}

func SetEnabled(ctx context.Context, short string, enabled, approved bool) error {
	short, err := authorize(ctx, short, true)
	if err != nil {
		return err
	}
	return plugin.SetEnabled(short, enabled, approved)
}

func Logs(ctx context.Context, short string) (string, error) {
	short, err := authorize(ctx, short, true)
	if err != nil {
		return "", err
	}
	return plugin.GetLogs(short), nil
}

func Delete(ctx context.Context, short string) error {
	short, err := authorize(ctx, short, true)
	if err != nil {
		return err
	}
	return plugin.Delete(short)
}

func Configuration(ctx context.Context, short string) (models.Configuration, map[string]any, error) {
	short, err := authorize(ctx, short, true)
	if err != nil {
		return models.Configuration{}, nil, err
	}
	manifest, err := plugin.Manifest(short)
	if err != nil {
		return models.Configuration{}, nil, manifestLookupError{cause: err}
	}
	values, err := plugin.GetConfiguration(short)
	return manifest.Configuration, values, err
}

func SaveConfiguration(ctx context.Context, short string, values map[string]any) error {
	short, err := authorize(ctx, short, true)
	if err != nil {
		return err
	}
	return plugin.SaveConfiguration(short, values)
}
