package connectapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/models"
	pluginapp "github.com/komari-monitor/komari/internal/plugin"
	pluginv1 "github.com/r11234567/komari-proto/gen/go/komari/plugin/v1"
	pluginv1connect "github.com/r11234567/komari-proto/gen/go/komari/plugin/v1/pluginv1connect"
	"google.golang.org/protobuf/types/known/structpb"
)

type pluginService struct {
	pluginv1connect.UnimplementedPluginServiceHandler
}

func (s *pluginService) ListPlugins(context.Context, *connect.Request[pluginv1.ListPluginsRequest]) (*connect.Response[pluginv1.ListPluginsResponse], error) {
	items := pluginapp.List()
	plugins := make([]*pluginv1.Plugin, 0, len(items))
	for _, item := range items {
		plugins = append(plugins, pluginInfoToProto(item))
	}
	return connect.NewResponse(&pluginv1.ListPluginsResponse{Plugins: plugins}), nil
}

func (s *pluginService) SetPluginEnabled(_ context.Context, req *connect.Request[pluginv1.SetPluginEnabledRequest]) (*connect.Response[pluginv1.SetPluginEnabledResponse], error) {
	short, err := requiredPluginShort(req.Msg.ShortName)
	if err != nil {
		return nil, err
	}
	err = pluginapp.SetEnabled(short, req.Msg.Enabled, req.Msg.PermissionsApproved)
	if errors.Is(err, pluginapp.ErrPermissionApprovalRequired) {
		return connect.NewResponse(&pluginv1.SetPluginEnabledResponse{RequiresPermissionApproval: true}), nil
	}
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pluginv1.SetPluginEnabledResponse{}), nil
}

func (s *pluginService) GetPluginLogs(_ context.Context, req *connect.Request[pluginv1.GetPluginLogsRequest]) (*connect.Response[pluginv1.GetPluginLogsResponse], error) {
	short, err := requiredPluginShort(req.Msg.ShortName)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pluginv1.GetPluginLogsResponse{Logs: pluginapp.GetLogs(short)}), nil
}

func (s *pluginService) DeletePlugin(_ context.Context, req *connect.Request[pluginv1.DeletePluginRequest]) (*connect.Response[pluginv1.DeletePluginResponse], error) {
	short, err := requiredPluginShort(req.Msg.ShortName)
	if err != nil {
		return nil, err
	}
	if err := pluginapp.Delete(short); err != nil {
		if errors.Is(err, pluginapp.ErrNotInstalled) {
			return nil, connectError(connect.CodeNotFound, err)
		}
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pluginv1.DeletePluginResponse{}), nil
}

func (s *pluginService) GetPluginConfiguration(_ context.Context, req *connect.Request[pluginv1.GetPluginConfigurationRequest]) (*connect.Response[pluginv1.GetPluginConfigurationResponse], error) {
	short, err := requiredPluginShort(req.Msg.ShortName)
	if err != nil {
		return nil, err
	}
	manifest, err := pluginapp.Manifest(short)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, err)
	}
	values, err := pluginapp.GetConfiguration(short)
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	encoded, err := structpb.NewStruct(values)
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pluginv1.GetPluginConfigurationResponse{
		Configuration: pluginConfigurationToProto(manifest.Configuration),
		Values:        encoded,
	}), nil
}

func (s *pluginService) SetPluginConfiguration(_ context.Context, req *connect.Request[pluginv1.SetPluginConfigurationRequest]) (*connect.Response[pluginv1.SetPluginConfigurationResponse], error) {
	short, err := requiredPluginShort(req.Msg.ShortName)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if req.Msg.Values != nil {
		values = req.Msg.Values.AsMap()
	}
	if err := pluginapp.SaveConfiguration(short, values); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pluginv1.SetPluginConfigurationResponse{}), nil
}

func requiredPluginShort(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", connectError(connect.CodeInvalidArgument, errors.New("plugin short name is required"))
	}
	return value, nil
}

func pluginInfoToProto(info pluginapp.Info) *pluginv1.Plugin {
	pages := make([]*pluginv1.PluginPage, 0, len(info.Pages))
	for _, page := range info.Pages {
		pages = append(pages, &pluginv1.PluginPage{
			File:       page.File,
			Title:      localizedTextToProto(page.Title),
			Icon:       page.Icon,
			Type:       string(page.Type),
			Url:        page.URL,
			Visibility: string(page.Visibility),
		})
	}
	permissions := info.Permissions
	return &pluginv1.Plugin{
		ShortName:               info.Short,
		Name:                    localizedTextToProto(info.Name),
		Description:             localizedTextToProto(info.Description),
		Author:                  localizedTextToProto(info.Author),
		Version:                 info.Version,
		Url:                     info.URL,
		Icon:                    info.Icon,
		KomariVersionConstraint: info.Komari,
		Entry:                   info.Entry,
		Permissions: &pluginv1.PluginPermissions{
			Node:                permissions.Node,
			AllowSystemRpc:      permissions.AllowSystemRPC,
			AllowRoutes:         permissions.AllowRoutes,
			AllowHooks:          permissions.AllowHooks,
			AllowHtmlInject:     permissions.AllowHTMLInject,
			AllowExec:           permissions.AllowExec,
			AllowListen:         permissions.AllowListen,
			AllowAllFileAccess:  permissions.AllowAllFileAccess,
			MaxHttpBodyBytes:    permissions.MaxHTTPBodyBytes,
			MaxChildOutputBytes: int64(permissions.MaxChildOutputBytes),
			TimeoutSeconds:      uint32(max(permissions.TimeoutSeconds, 0)),
		},
		Configuration: pluginConfigurationToProto(info.Configuration),
		Pages:         pages,
		Enabled:       info.Enabled,
		Running:       info.Running,
		LastError:     info.LastError,
	}
}

func localizedTextToProto(value any) *pluginv1.LocalizedText {
	result := &pluginv1.LocalizedText{}
	switch value := value.(type) {
	case string:
		result.Fallback = value
	case map[string]string:
		result.Translations = value
	case map[string]any:
		result.Translations = make(map[string]string, len(value))
		for key, item := range value {
			if text, ok := item.(string); ok {
				result.Translations[key] = text
			}
		}
	}
	return result
}

func pluginConfigurationToProto(configuration models.Configuration) *pluginv1.PluginConfiguration {
	result := &pluginv1.PluginConfiguration{
		Type: configuration.Type,
		Icon: configuration.Icon,
		Name: localizedTextToProto(configuration.Name),
	}
	raw, err := json.Marshal(configuration.Data)
	if err != nil {
		return result
	}
	var items []models.ManagedThemeConfigurationItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return result
	}
	for _, item := range items {
		defaultValue, err := structpb.NewValue(item.Default)
		if err != nil {
			defaultValue = structpb.NewNullValue()
		}
		result.Items = append(result.Items, &pluginv1.PluginConfigurationItem{
			Key:          item.Key,
			Name:         localizedTextToProto(item.Name),
			Required:     item.Required,
			Type:         item.Type,
			Options:      item.Options,
			DefaultValue: defaultValue,
			Help:         localizedTextToProto(item.Help),
		})
	}
	return result
}
