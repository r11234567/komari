package connectapi

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/internal/credentialaction"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/config"
	jsonRpc "github.com/komari-monitor/komari/web/rpc/jsonrpc"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestTerminalCSSRequiresProofForExactChange(t *testing.T) {
	ctx, _, _ := actionFixture(t)
	previous, err := jsonRpc.GetXtermjsSettings()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, config.Set(config.XtermjsSettingsKey, previous))
	})
	service := &terminalSettingsService{}
	const css = ".xterm { opacity: 0.9; }"
	token := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("terminal:custom-css", css))
	request := func(value, proof string) *connect.Request[adminv1.SetTerminalSettingsRequest] {
		req := connect.NewRequest(&adminv1.SetTerminalSettingsRequest{Settings: &adminv1.TerminalSettings{FontSize: 18, CustomCss: value}})
		req.Header().Set(credentialaction.Header, proof)
		return req
	}
	_, err = service.SetTerminalSettings(ctx, request(css, ""))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	_, err = service.SetTerminalSettings(ctx, request(css+" ", token))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	updated, err := service.SetTerminalSettings(ctx, request(css, token))
	require.NoError(t, err)
	require.Equal(t, css, updated.Msg.Settings.CustomCss)
	_, err = service.SetTerminalSettings(ctx, request(css, ""))
	require.NoError(t, err)
	_, err = service.SetTerminalSettings(ctx, request("", ""))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
}

func TestTerminalSettingsProtoRoundTrip(t *testing.T) {
	theme, err := structpb.NewStruct(map[string]any{
		"background": "#101010",
		"foreground": "#eeeeee",
	})
	if err != nil {
		t.Fatalf("build theme: %v", err)
	}
	original := &adminv1.TerminalSettings{
		CursorBlink:           true,
		ConvertEol:            false,
		FontFamily:            "Test Mono",
		FontSize:              18,
		MacOptionIsMeta:       true,
		Scrollback:            1000,
		Padding:               12,
		TransparentBackground: true,
		CustomCss:             ".xterm { opacity: 0.9; }",
		Theme:                 theme,
	}

	settings, err := terminalSettingsFromProto(original)
	if err != nil {
		t.Fatalf("from proto: %v", err)
	}
	normalized, err := jsonRpc.NormalizeXtermjsSettings(settings)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	roundTripped, err := terminalSettingsToProto(normalized)
	if err != nil {
		t.Fatalf("to proto: %v", err)
	}

	if roundTripped.FontFamily != original.FontFamily ||
		roundTripped.FontSize != original.FontSize ||
		roundTripped.Scrollback != original.Scrollback ||
		roundTripped.Padding != original.Padding ||
		roundTripped.TransparentBackground != original.TransparentBackground ||
		roundTripped.CustomCss != original.CustomCss {
		t.Fatalf("settings changed across the round trip: %+v", roundTripped)
	}
	if roundTripped.Theme == nil || roundTripped.Theme.AsMap()["background"] != "#101010" {
		t.Fatalf("theme lost across the round trip: %+v", roundTripped.Theme)
	}
}
