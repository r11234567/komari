package connectapi

import (
	"testing"

	jsonRpc "github.com/komari-monitor/komari/web/rpc/jsonrpc"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

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
