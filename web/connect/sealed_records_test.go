package connectapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/internal/credentialaction"
	"github.com/komari-monitor/komari/internal/diagnosticrecords"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/r11234567/komari-proto/diagnostics"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestSubmitSealedRecordsBindsRegisteredNode(t *testing.T) {
	node := uuid.New()
	public, signing, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	archive := filepath.Join(local, "records")
	if err := os.Mkdir(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(local, "receipt-key")
	if err := os.WriteFile(key, signing, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOMARI_PANEL_DIAGNOSTIC_NODE", node.String())
	t.Setenv("KOMARI_PANEL_DIAGNOSTIC_SIGNING_KEY", key)
	t.Setenv("KOMARI_PANEL_DIAGNOSTIC_ARCHIVE", archive)
	panelPublic, panelPrivate, err := diagnostics.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	header := diagnostics.Header{Node: node, Stream: uuid.New(), Sequence: 1}
	data := []byte("generated diagnostic fixture")
	sealed, err := diagnostics.Seal(panelPublic, header, data)
	if err != nil {
		t.Fatal(err)
	}
	req := func() *connect.Request[wrapperspb.BytesValue] { return connect.NewRequest(wrapperspb.Bytes(sealed)) }
	if _, err := submitSealedRecord(context.Background(), req()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated upload: %v", err)
	}
	ctx := func(id string) context.Context {
		return rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{
			Principal: &rpc.Principal{Type: rpc.PrincipalAgent, ClientUUID: id},
		})
	}
	if _, err := submitSealedRecord(ctx(uuid.NewString()), req()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("different enrolled node: %v", err)
	}
	accepted, err := submitSealedRecord(ctx(node.String()), req())
	if err != nil {
		t.Fatal(err)
	}
	var receipt diagnostics.Receipt
	if err := json.Unmarshal(accepted.Msg.Value, &receipt); err != nil || receipt.Verify(public, sealed) != nil {
		t.Fatalf("verify panel receipt: %v", err)
	}
	again, err := submitSealedRecord(ctx(node.String()), req())
	if err != nil || !bytes.Equal(again.Msg.GetValue(), accepted.Msg.GetValue()) {
		t.Fatalf("resubmission changed receipt: %v", err)
	}
	entries, err := os.ReadDir(archive)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".tar.gz") {
		t.Fatalf("archive entries = %d, err = %v", len(entries), err)
	}
	disk, err := os.ReadFile(filepath.Join(archive, entries[0].Name()))
	if err != nil || bytes.Contains(disk, data) {
		t.Fatalf("unexpected archive content: %v", err)
	}
	_, receiptOnDisk, envelope, err := diagnosticrecords.VerifyBundle(bytes.NewReader(disk), public)
	if err != nil || !bytes.Equal(envelope, sealed) || !bytes.Equal(receiptOnDisk.Signature, receipt.Signature) {
		t.Fatalf("archive signature or envelope failed verification: %v", err)
	}
	_, recovered, err := diagnostics.Open(panelPrivate, envelope)
	if err != nil || !bytes.Equal(data, recovered) {
		t.Fatalf("panel private key could not open archive: %v", err)
	}
	admin, _, _ := actionFixture(t)
	listReq := connect.NewRequest(wrapperspb.Bytes(nil))
	if _, err := listSealedRecords(ctx(node.String()), listReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent could list private archives: %v", err)
	}
	listed, err := listSealedRecords(admin, listReq)
	if err != nil {
		t.Fatal(err)
	}
	var index []diagnosticrecords.Entry
	if err := json.Unmarshal(listed.Msg.Value, &index); err != nil || len(index) != 1 || index[0].Name != entries[0].Name() {
		t.Fatalf("invalid archive index: %v", err)
	}
	download := connect.NewRequest(wrapperspb.Bytes([]byte(index[0].Name)))
	if _, err := downloadSealedRecord(admin, download); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("download without fresh credential proof: %v", err)
	}
	proof := actionAuthorization(t, admin, credential.SensitiveOperation, credentialaction.ScopedResource("forensics:export", index[0].Name))
	download.Header().Set(credentialaction.Header, proof)
	down, err := downloadSealedRecord(admin, download)
	if err != nil || !bytes.Equal(down.Msg.GetValue(), disk) {
		t.Fatalf("authorized download changed the original archive: %v", err)
	}
	if _, err := downloadSealedRecord(admin, download); err == nil {
		t.Fatal("replayed evidence export confirmation")
	}
}
