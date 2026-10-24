package connectapi

import (
	"context"
	"encoding/json"
	"errors"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/pkg/rpc"
	jsonRpc "github.com/komari-monitor/komari/web/rpc/jsonrpc"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
	adminv1connect "github.com/r11234567/komari-proto/gen/go/komari/admin/v1/adminv1connect"
	"google.golang.org/protobuf/types/known/structpb"
)

// terminalSettingsService owns the deployment-wide terminal appearance.
//
// The legacy RPC2 handler remains for the REST bridge, but new clients use
// this Connect service. Both share the same normalization and storage so the
// two surfaces cannot drift.
type terminalSettingsService struct {
	adminv1connect.UnimplementedTerminalSettingsServiceHandler
}

func (s *terminalSettingsService) GetTerminalSettings(_ context.Context, _ *connect.Request[adminv1.GetTerminalSettingsRequest]) (*connect.Response[adminv1.GetTerminalSettingsResponse], error) {
	settings, err := jsonRpc.GetXtermjsSettings()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	message, err := terminalSettingsToProto(settings)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adminv1.GetTerminalSettingsResponse{Settings: message}), nil
}

func (s *terminalSettingsService) SetTerminalSettings(ctx context.Context, req *connect.Request[adminv1.SetTerminalSettingsRequest]) (*connect.Response[adminv1.SetTerminalSettingsResponse], error) {
	if req.Msg.Settings == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("settings are required"))
	}
	input, err := terminalSettingsFromProto(req.Msg.Settings)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	normalized, err := jsonRpc.NormalizeXtermjsSettings(input)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := jsonRpc.SaveXtermjsSettings(normalized); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	message, err := terminalSettingsToProto(normalized)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if meta := rpc.MetaFromContext(ctx); meta != nil {
		auditlog.Log(meta.RemoteIP, meta.UserUUID, "update terminal settings", "info")
	}
	return connect.NewResponse(&adminv1.SetTerminalSettingsResponse{Settings: message}), nil
}

func terminalSettingsToProto(settings jsonRpc.XtermJSSettings) (*adminv1.TerminalSettings, error) {
	out := &adminv1.TerminalSettings{
		TransparentBackground: settings.TransparentBackground,
		CustomCss:             settings.CustomCss,
	}
	if options := settings.TerminalOptions; options != nil {
		out.FontFamily = options.FontFamily
		out.FontSize = uint32(max(options.FontSize, 0))
		if options.CursorBlink != nil {
			out.CursorBlink = *options.CursorBlink
		}
		if options.ConvertEol != nil {
			out.ConvertEol = *options.ConvertEol
		}
		if options.MacOptionIsMeta != nil {
			out.MacOptionIsMeta = *options.MacOptionIsMeta
		}
		if options.Scrollback != nil {
			out.Scrollback = uint32(max(*options.Scrollback, 0))
		}
		if options.Theme != nil {
			raw, err := json.Marshal(options.Theme)
			if err != nil {
				return nil, err
			}
			var values map[string]any
			if err := json.Unmarshal(raw, &values); err != nil {
				return nil, err
			}
			theme, err := structpb.NewStruct(values)
			if err != nil {
				return nil, err
			}
			out.Theme = theme
		}
	}
	if settings.TerminalPadding != nil {
		out.Padding = uint32(max(*settings.TerminalPadding, 0))
	}
	return out, nil
}

func terminalSettingsFromProto(in *adminv1.TerminalSettings) (jsonRpc.XtermJSSettings, error) {
	cursorBlink := in.CursorBlink
	convertEol := in.ConvertEol
	macOptionIsMeta := in.MacOptionIsMeta
	fontSize := int(in.FontSize)
	scrollback := int(in.Scrollback)
	padding := int(in.Padding)
	out := jsonRpc.XtermJSSettings{
		TerminalOptions: &jsonRpc.TerminalOptions{
			CursorBlink:     &cursorBlink,
			ConvertEol:      &convertEol,
			FontFamily:      in.FontFamily,
			FontSize:        fontSize,
			MacOptionIsMeta: &macOptionIsMeta,
			Scrollback:      &scrollback,
		},
		TerminalPadding:       &padding,
		TransparentBackground: in.TransparentBackground,
		CustomCss:             in.CustomCss,
	}
	if in.Theme != nil {
		raw, err := json.Marshal(in.Theme.AsMap())
		if err != nil {
			return jsonRpc.XtermJSSettings{}, err
		}
		var theme jsonRpc.ThemeConfig
		if err := json.Unmarshal(raw, &theme); err != nil {
			return jsonRpc.XtermJSSettings{}, err
		}
		out.TerminalOptions.Theme = &theme
	}
	return out, nil
}
