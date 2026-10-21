package connectapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/pkg/rpc"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	"gorm.io/gorm"
)

func TestHostLookupOnlyReturnsNotFoundForAbsentOperation(t *testing.T) {
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal("missing-host-operation")})
	request := connect.NewRequest(&hostv1.GetHostOperationRequest{AgentId: "missing-host-operation", OperationId: "never-claimed"})
	service := &hostService{}
	_, err := service.GetHostOperation(ctx, request)
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("absent operation: %v", err)
	}
	db := dbcore.GetDBInstance()
	const callback = "test:host-read-failure"
	if err = db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) { tx.AddError(errors.New("injected database read outage")) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback) })
	_, err = service.GetHostOperation(ctx, request)
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("database outage must not prove an operation is unclaimed: %v", err)
	}
}
