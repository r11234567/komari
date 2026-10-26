package plugin

import (
	"bytes"
	"strings"
	"testing"

	moejs "github.com/Calcium-Ion/moejs"
	"github.com/Calcium-Ion/moejs/engine"
	"github.com/komari-monitor/komari/pkg/jsruntime/console"
	"github.com/komari-monitor/komari/pkg/jsruntime/native"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestServerRPCErrorLogsIncludeMessage(t *testing.T) {
	vm := moejs.NewRuntime(moejs.Options{})
	var output bytes.Buffer
	if err := console.New(&output).Inject(vm); err != nil {
		t.Fatal(err)
	}
	value := engine.ObjectValue(jsRPCError(vm, &rpc.JsonRpcError{Code: rpc.InvalidParams, Message: "Invalid notification parameters"}))
	if err := vm.SetGlobal("rpcFailure", value); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Run(vm, `console.error("synthetic send failure:", rpcFailure);`); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"synthetic send failure", "Invalid notification parameters", `"code":-32602`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("RPC error log lost %q: %s", want, output.String())
		}
	}
}
