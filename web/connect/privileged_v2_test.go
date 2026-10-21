package connectapi

import (
	"bytes"
	"crypto/ed25519"
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
