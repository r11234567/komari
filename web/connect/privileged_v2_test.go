package connectapi

import (
	"bytes"
	"connectrpc.com/connect"
	"context"
	"crypto/ed25519"
	"github.com/komari-monitor/komari/pkg/rpc"
	configv1connect "github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPrivilegedV2SignatureCoversApprovalAndLease(t *testing.T) {
	now := time.Now().UTC()
	expiry := now.Add(time.Minute)
	auth := &hostv1.HostAuthorization{AgentId: "signature-fixture", Revision: 1, ConfigHash: "fixture-hash", ApprovalGeneration: 1, ServiceId: "fixture.service", TargetPrivilegeMode: 1, Actions: []hostv1.HostAction{1}, Nonce: "fixture-nonce", IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expiry), ApprovedBy: "admin"}
	data, err := proto.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signedPrivilegedRevision(privilegeddelivery.Revision{Client: auth.AgentId, Revision: 1, ProtocolVersion: 2, ConfigHash: auth.ConfigHash, ApprovalGeneration: 1, State: 2, SavedAt: now, ConfirmedAt: &now, ConfirmedBy: "admin", Authorization: data, Nonce: auth.Nonce, NonceExpires: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	key, err := enrollment.ActiveSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(key.PublicKey, signed.Signature.Payload, signed.Signature.Signatures[0].Value) {
		t.Fatal("invalid revision signature")
	}
	var instruction securityv1.SignedInstruction
	if err := proto.Unmarshal(signed.Signature.Payload, &instruction); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*configv1.PrivilegedRevision){
		"state":      func(m *configv1.PrivilegedRevision) { m.State = 3 },
		"approval":   func(m *configv1.PrivilegedRevision) { m.ConfirmedBy = "other" },
		"generation": func(m *configv1.PrivilegedRevision) { m.ApprovalGeneration++ },
		"service":    func(m *configv1.PrivilegedRevision) { m.Authorization.ServiceId = "other" },
		"expiry": func(m *configv1.PrivilegedRevision) {
			m.Authorization.ExpiresAt = timestamppb.New(expiry.Add(time.Hour))
		},
		"action": func(m *configv1.PrivilegedRevision) { m.Authorization.Actions = []hostv1.HostAction{2} },
	} {
		t.Run(name, func(t *testing.T) {
			copy := proto.Clone(signed).(*configv1.PrivilegedRevision)
			copy.Signature = nil
			mutate(copy)
			body, err := proto.MarshalOptions{Deterministic: true}.Marshal(copy)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(body, instruction.Body) {
				t.Fatal("mutated approval still matches signed body")
			}
		})
	}
	op := &hostv1.HostOperation{OperationId: "fixture-operation", Authorization: auth, Phase: 1, LeaseExpiresAt: timestamppb.New(expiry)}
	if err := signHostOperation(op); err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(key.PublicKey, op.Signature.Payload, op.Signature.Signatures[0].Value) {
		t.Fatal("invalid host signature")
	}
	if err := proto.Unmarshal(op.Signature.Payload, &instruction); err != nil {
		t.Fatal(err)
	}
	copy := proto.Clone(op).(*hostv1.HostOperation)
	copy.Signature = nil
	copy.LeaseExpiresAt = timestamppb.New(expiry.Add(time.Hour))
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(copy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(body, instruction.Body) {
		t.Fatal("lease expiry is not protected")
	}
}
func TestPrivilegedV2RejectsUnsupportedRuntimeFields(t *testing.T) {
	s := "192.0.2.53"
	p := clients.DeploymentProfile{}
	if err := applyRuntime(&p, &configv1.RuntimeConfig{CustomDns: &s}); err == nil {
		t.Fatal("custom DNS silently discarded")
	}
}

func TestCompletedHostReadDoesNotRenewExecutionLease(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	op := &hostv1.HostOperation{OperationId: "old-completed", Authorization: &hostv1.HostAuthorization{AgentId: "node"}, Phase: 3, LeaseExpiresAt: timestamppb.New(expired)}
	if err := signHostOperation(op); err != nil {
		t.Fatal(err)
	}
	var payload securityv1.SignedInstruction
	if err := proto.Unmarshal(op.Signature.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.ExpiresAt.AsTime().After(time.Now()) {
		t.Fatal("completed snapshot cannot be read after a network outage")
	}
	if !op.LeaseExpiresAt.AsTime().Equal(expired) {
		t.Fatal("reading completed operation extended execution lease")
	}
}

func TestPrivilegedV2LegacyProfileSaveCannotOverwriteInstallation(t *testing.T) {
	id, _, err := clients.CreateClient()
	if err != nil {
		t.Fatal(err)
	}
	profile, _, err := clients.GetDeploymentProfile(id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = privilegeddelivery.Negotiate(dbcore.GetDBInstance(), id, "profile-fixture.service", "boot", 2, []int32{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	changed := profile
	changed.RuntimeIdentity = clients.AgentRuntimeIdentityPrivileged
	changed.RemoteControlEnabled = true
	if _, err := clients.SaveDeploymentProfile(id, changed); err == nil {
		t.Fatal("legacy deployment save overwrote v2 installation fields")
	}
	profile.EnableInterval = true
	profile.Interval = 7
	if _, err := clients.SaveDeploymentProfile(id, profile); err != nil {
		t.Fatalf("ordinary runtime save must remain available: %v", err)
	}
}

func TestPrivilegedV2WatchDeliversApprovalOfAlreadyReceivedRevision(t *testing.T) {
	id, _, err := clients.CreateClient()
	if err != nil {
		t.Fatal(err)
	}
	_, err = privilegeddelivery.Negotiate(dbcore.GetDBInstance(), id, "watch.service", "boot", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := privilegeddelivery.Save(id, privilegeddelivery.Settings{RequiredPrivilegeMode: 2}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := privilegeddelivery.Confirm(id, saved.Revision, "admin")
	if err != nil {
		t.Fatal(err)
	}
	path, handler := configv1connect.NewPrivilegedDeliveryServiceHandler(&privilegedDeliveryService{})
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := rpc.NewContextWithMeta(r.Context(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(id)})
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := configv1connect.NewPrivilegedDeliveryServiceClient(server.Client(), server.URL)
	stream, err := client.WatchPrivilegedDelivery(ctx, connect.NewRequest(&configv1.WatchPrivilegedDeliveryRequest{AgentId: id, AfterRevision: saved.Revision, AfterSequence: saved.UpdateSequence, ProtocolVersion: 2}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() {
		t.Fatalf("approval of same revision was not delivered: %v", stream.Err())
	}
	if stream.Msg().Revision.Revision != saved.Revision || stream.Msg().UpdateSequence != approved.UpdateSequence {
		t.Fatalf("wrong stream cursor: %v", stream.Msg())
	}
}
func TestPrivilegedV2ListReturnsAppliedOutsideHistoryPage(t *testing.T) {
	id, _, err := clients.CreateClient()
	if err != nil {
		t.Fatal(err)
	}
	_, err = privilegeddelivery.Negotiate(dbcore.GetDBInstance(), id, "history.service", "boot", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := privilegeddelivery.Save(id, privilegeddelivery.Settings{RequiredPrivilegeMode: 2}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := privilegeddelivery.Confirm(id, first.Revision, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = privilegeddelivery.Observe(dbcore.GetDBInstance(), id, "history-ack", &hostv1.DaemonObservation{Revision: 1, ConfigHash: approved.ConfigHash, ApprovalGeneration: approved.ApprovalGeneration, ServiceId: "history.service", StartupId: "boot", PrivilegeMode: 2, ObservedAt: timestamppb.Now(), EffectiveCapabilities: &reportv1.AgentCapabilities{PrivilegeMode: 2}})
	if err != nil {
		t.Fatal(err)
	}
	for n := uint64(1); n < 8; n++ {
		if _, err := privilegeddelivery.Save(id, privilegeddelivery.Settings{RequiredPrivilegeMode: 2}, n, ""); err != nil {
			t.Fatal(err)
		}
	}
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("admin")})
	result, err := (&privilegedDeliveryService{}).ListPrivilegedRevisions(ctx, connect.NewRequest(&configv1.ListPrivilegedRevisionsRequest{AgentId: id, Limit: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Msg.Revisions) != 1 || result.Msg.Revisions[0].Revision != 8 || result.Msg.AppliedRevision != 1 || result.Msg.Applied == nil || result.Msg.Applied.Revision != 1 {
		t.Fatalf("applied snapshot lost to pagination: %v", result.Msg)
	}
}
func TestPrivilegedV2ConfirmationRetryReadsExistingApproval(t *testing.T) {
	id, _, err := clients.CreateClient()
	if err != nil {
		t.Fatal(err)
	}
	_, err = privilegeddelivery.Negotiate(dbcore.GetDBInstance(), id, "retry.service", "boot", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := privilegeddelivery.Save(id, privilegeddelivery.Settings{RequiredPrivilegeMode: 2}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := privilegeddelivery.Confirm(id, saved.Revision, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("admin")})
	response, err := (&privilegedDeliveryService{}).ConfirmPrivilegedDelivery(ctx, connect.NewRequest(&configv1.ConfirmPrivilegedDeliveryRequest{AgentId: id, Revision: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if response.Msg.Revision.ApprovalGeneration != approved.ApprovalGeneration || !response.Msg.Revision.Authorization.ExpiresAt.AsTime().Equal(*approved.NonceExpires) {
		t.Fatal("retry renewed approval")
	}
}
