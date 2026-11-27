package connectapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/internal/credentialaction"
	"github.com/komari-monitor/komari/internal/diagnosticrecords"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/r11234567/komari-proto/diagnostics"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	sealedRecordProcedure   = "/komari.diag.v1.SealedRecordService/Submit"
	listSealedProcedure     = "/komari.diag.v1.SealedRecordService/List"
	downloadSealedProcedure = "/komari.diag.v1.SealedRecordService/Download"
)

func sealedRecordHandler(options ...connect.HandlerOption) http.Handler {
	return connect.NewUnaryHandler(sealedRecordProcedure, submitSealedRecord, options...)
}

func submitSealedRecord(ctx context.Context, request *connect.Request[wrapperspb.BytesValue]) (*connect.Response[wrapperspb.BytesValue], error) {
	node, err := requireAgent(rpc.MetaFromContext(ctx), "")
	if err != nil {
		return nil, err
	}
	if node != os.Getenv("KOMARI_PANEL_DIAGNOSTIC_NODE") {
		return nil, connectError(connect.CodePermissionDenied, nil)
	}
	identity, err := uuid.Parse(node)
	if err != nil {
		return nil, connectError(connect.CodePermissionDenied, nil)
	}
	archive, err := openDiagnosticArchive()
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	defer archive.Close()
	receipt, err := archive.Put(identity, request.Msg.GetValue())
	switch {
	case errors.Is(err, diagnostics.ErrEnvelope):
		return nil, connectError(connect.CodeInvalidArgument, nil)
	case errors.Is(err, diagnosticrecords.ErrIdentity):
		return nil, connectError(connect.CodePermissionDenied, nil)
	case errors.Is(err, diagnosticrecords.ErrConflict):
		return nil, connectError(connect.CodeAlreadyExists, nil)
	case err != nil:
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return nil, connectError(connect.CodeInternal, nil)
	}
	return connect.NewResponse(wrapperspb.Bytes(data)), nil
}

func openDiagnosticArchive() (*diagnosticrecords.Archive, error) {
	key, err := diagnosticSigningKey(os.Getenv("KOMARI_PANEL_DIAGNOSTIC_SIGNING_KEY"))
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return diagnosticrecords.Open(os.Getenv("KOMARI_PANEL_DIAGNOSTIC_ARCHIVE"), key)
}

func requireDiagnosticAdmin(ctx context.Context) error {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil || meta.Principal.Type != rpc.PrincipalUser || meta.SessionToken == "" || !meta.Principal.HasRole(rpc.RoleAdmin) {
		return connectError(connect.CodePermissionDenied, errors.New("an administrator browser session is required"))
	}
	return nil
}

func listSealedRecords(ctx context.Context, _ *connect.Request[wrapperspb.BytesValue]) (*connect.Response[wrapperspb.BytesValue], error) {
	if err := requireDiagnosticAdmin(ctx); err != nil {
		return nil, err
	}
	if os.Getenv("KOMARI_PANEL_DIAGNOSTIC_ARCHIVE") == "" {
		return diagnosticReply([]byte("[]")), nil
	}
	archive, err := openDiagnosticArchive()
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	defer archive.Close()
	entries, err := archive.List()
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return nil, connectError(connect.CodeInternal, nil)
	}
	return diagnosticReply(encoded), nil
}

func downloadSealedRecord(ctx context.Context, request *connect.Request[wrapperspb.BytesValue]) (*connect.Response[wrapperspb.BytesValue], error) {
	if err := requireDiagnosticAdmin(ctx); err != nil {
		return nil, err
	}
	name := string(request.Msg.GetValue())
	if !diagnosticrecords.ValidName(name) {
		return nil, connectError(connect.CodeInvalidArgument, nil)
	}
	if request.Header().Get(credentialaction.Header) == "" {
		return nil, connectError(connect.CodePermissionDenied, errors.New("fresh credential authorization is required"))
	}
	if err := verifyConnectHeaderCredential(ctx, request.Header(), "forensics:export", name); err != nil {
		return nil, err
	}
	archive, err := openDiagnosticArchive()
	if err != nil {
		return nil, connectError(connect.CodeUnavailable, nil)
	}
	defer archive.Close()
	data, err := archive.Download(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, connectError(connect.CodeNotFound, nil)
	}
	if err != nil {
		return nil, connectError(connect.CodeDataLoss, nil)
	}
	return diagnosticReply(data), nil
}

func diagnosticReply(data []byte) *connect.Response[wrapperspb.BytesValue] {
	reply := connect.NewResponse(wrapperspb.Bytes(data))
	reply.Header().Set("Cache-Control", "no-store")
	return reply
}

func diagnosticSigningKey(path string) (ed25519.PrivateKey, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, diagnostics.ErrReceipt
	}
	key, err := io.ReadAll(io.LimitReader(file, ed25519.PrivateKeySize+1))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		clear(key)
		return nil, diagnostics.ErrReceipt
	}
	return ed25519.PrivateKey(key), nil
}
