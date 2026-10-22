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
	hostv1connect "github.com/r11234567/komari-proto/gen/go/komari/host/v1/hostv1connect"
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

// A restarted daemon negotiates a new node cursor without changing the
// approved configuration row. Watch and the mandatory unary recheck must
// agree, or the daemon can neither apply nor acknowledge the host result.
func TestPrivilegedV2RestartKeepsWatchAndRevalidationConsistent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		from   int32
		to     int32
		helper bool
	}{
		{name: "same identity", from: 2, to: 2},
		{name: "promote", from: 2, to: 1},
		{name: "demote", from: 1, to: 2},
		{name: "separate rescue", from: 2, to: 2, helper: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, _, err := clients.CreateClient()
			if err != nil {
				t.Fatal(err)
			}
			db := dbcore.GetDBInstance()
			_, err = privilegeddelivery.Negotiate(db, id, "restart.service", "before", tc.from, []int32{1, 2, 3})
			if err != nil {
				t.Fatal(err)
			}
			saved, err := privilegeddelivery.Save(id, privilegeddelivery.Settings{RequiredPrivilegeMode: tc.to, RescueHelperEnabled: tc.helper}, 0, "")
			if err != nil {
				t.Fatal(err)
			}
			approved, err := privilegeddelivery.Confirm(id, saved.Revision, "admin")
			if err != nil {
				t.Fatal(err)
			}
			authorization, err := privilegeddelivery.Authorization(approved)
			if err != nil {
				t.Fatal(err)
			}
			operationID := ""
			if len(authorization.Actions) > 0 {
				operationID = "restart-operation"
				_, err = privilegeddelivery.PrepareOperation(db, &hostv1.PrepareHostOperationRequest{AgentId: id, OperationId: operationID, Revision: approved.Revision, ConfigHash: approved.ConfigHash, ApprovalGeneration: approved.ApprovalGeneration, Nonce: approved.Nonce})
				if err != nil {
					t.Fatal(err)
				}
				_, err = privilegeddelivery.ReportOperation(db, &hostv1.ReportHostOperationRequest{AgentId: id, OperationId: operationID, ReceiptId: "host-local-result", Phase: hostv1.HostOperationPhase_HOST_OPERATION_PHASE_LOCAL_APPLIED, Operator: "admin", LocalAuthentication: "pam"})
				if err != nil {
					t.Fatal(err)
				}
			}
			mux := http.NewServeMux()
			register := func(path string, handler http.Handler) {
				mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := rpc.NewContextWithMeta(r.Context(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(id)})
					handler.ServeHTTP(w, r.WithContext(ctx))
				}))
			}
			register(configv1connect.NewPrivilegedDeliveryServiceHandler(&privilegedDeliveryService{}))
			register(hostv1connect.NewHostServiceHandler(&hostService{}))
			server := httptest.NewServer(mux)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			host := hostv1connect.NewHostServiceClient(server.Client(), server.URL)
			negotiated, err := host.NegotiateHostProtocol(ctx, connect.NewRequest(&hostv1.NegotiateHostProtocolRequest{AgentId: id, ProtocolVersion: 2, ServiceId: "restart.service", StartupId: "after", PrivilegeMode: reportv1.PrivilegeMode(tc.to), SupportedActions: []hostv1.HostAction{1, 2, 3}}))
			if err != nil {
				t.Fatal(err)
			}
			client := configv1connect.NewPrivilegedDeliveryServiceClient(server.Client(), server.URL)
			stream, err := client.WatchPrivilegedDelivery(ctx, connect.NewRequest(&configv1.WatchPrivilegedDeliveryRequest{AgentId: id, ProtocolVersion: 2, AfterSequence: approved.UpdateSequence}))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if !stream.Receive() {
				t.Fatalf("missing restarted snapshot: %v", stream.Err())
			}
			watched := proto.Clone(stream.Msg().Revision).(*configv1.PrivilegedRevision)
			fetched, err := client.GetPrivilegedDelivery(ctx, connect.NewRequest(&configv1.GetPrivilegedDeliveryRequest{AgentId: id}))
			if err != nil {
				t.Fatal(err)
			}
			current := fetched.Msg.Revision
			if watched.GetSignature() == nil || current.GetSignature() == nil || watched.GetUpdateSequence() != negotiated.Msg.UpdateSequence || current.GetUpdateSequence() != watched.GetUpdateSequence() {
				t.Fatalf("restart cursor disagrees: negotiation=%v watch=%v current=%v", negotiated.Msg, watched, current)
			}
			watched.Signature, current.Signature = nil, nil
			if !proto.Equal(watched, current) {
				t.Fatalf("unary approval revalidation disagrees with watch: watch=%v current=%v", watched, current)
			}
			snapshot, err := privilegeddelivery.GetState(id)
			if err != nil || snapshot.State.AppliedRevision != 0 {
				t.Fatalf("host result fabricated daemon completion: %v %+v", err, snapshot.State)
			}
			ack, err := client.ReportPrivilegedDelivery(ctx, connect.NewRequest(&configv1.ReportPrivilegedDeliveryRequest{AgentId: id, Revision: current.Revision, ProtocolVersion: 2, State: configv1.PrivilegedDeliveryState_PRIVILEGED_DELIVERY_STATE_DELIVERED, ReceiptId: "daemon-ack", ConfigHash: current.ConfigHash, ApprovalGeneration: current.ApprovalGeneration, Observation: &hostv1.DaemonObservation{Revision: current.Revision, ConfigHash: current.ConfigHash, ApprovalGeneration: current.ApprovalGeneration, OperationId: operationID, ServiceId: "restart.service", StartupId: "after", PrivilegeMode: reportv1.PrivilegeMode(tc.to), ObservedAt: timestamppb.Now(), EffectiveCapabilities: &reportv1.AgentCapabilities{PrivilegeMode: reportv1.PrivilegeMode(tc.to), RescueHelper: &reportv1.CapabilityState{Available: tc.helper}}, HelperInstalled: tc.helper, HelperRunning: tc.helper, HelperAuthorized: tc.helper, HelperLeaseReady: tc.helper}}))
			if err != nil || !ack.Msg.GetAccepted() {
				t.Fatalf("daemon ACK failed after revalidation: %v", err)
			}
			filtered, err := client.GetPrivilegedDelivery(ctx, connect.NewRequest(&configv1.GetPrivilegedDeliveryRequest{AgentId: id, AppliedRevision: current.Revision}))
			if err != nil || filtered.Msg.Revision != nil {
				t.Fatalf("applied revision filter failed: %v", err)
			}
		})
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
