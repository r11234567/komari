package connectapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/pkg/rpc"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	configconnect "github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func rescueWatchFixture(t *testing.T) (string, string, privilegeddelivery.Revision, http.Handler) {
	t.Helper()
	id, _, err := clients.CreateClient()
	if err != nil {
		t.Fatal(err)
	}
	db := dbcore.GetDBInstance()
	token := uuid.NewString()
	digest := sha256.Sum256([]byte(token))
	credential := models.AgentCredential{Client: id, AccessTokenHash: hex.EncodeToString(digest[:]), AccessExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := privilegeddelivery.Negotiate(db, id, "test.service", "boot", 2, []int32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	saved, err := privilegeddelivery.Save(id, privilegeddelivery.Settings{RequiredPrivilegeMode: 2, RescueHelperEnabled: true}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	path, handler := configconnect.NewPrivilegedDeliveryServiceHandler(&privilegedDeliveryService{}, connect.WithInterceptors(newPolicyInterceptor()))
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(rpc.NewContextWithMeta(r.Context(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(id)})))
	}))
	return id, token, saved, mux
}

func TestRescueWatchFreshSignedHeartbeatAndCredentialRevocation(t *testing.T) {
	id, token, saved, mux := rescueWatchFixture(t)
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	client := configconnect.NewPrivilegedDeliveryServiceClient(server.Client(), server.URL)
	req := connect.NewRequest(&configv1.WatchPrivilegedDeliveryRequest{AgentId: id, ProtocolVersion: 2})
	req.Header().Set("Authorization", "Bearer "+token)
	req.Header().Set(rescueWatchHeader, "v1")
	stream, err := client.WatchPrivilegedDelivery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var previous *securityv1.SignedInstruction
	for i := 0; i < 2; i++ {
		if !stream.Receive() {
			t.Fatalf("signed heartbeat %d missing: %v", i, stream.Err())
		}
		if stream.ResponseHeader().Get(rescueWatchHeader) != "v1" {
			t.Fatal("missing heartbeat negotiation")
		}
		revision := stream.Msg().Revision
		if revision.Revision != saved.Revision || revision.Signature == nil {
			t.Fatal("missing signed current revision")
		}
		var signed securityv1.SignedInstruction
		if err := proto.Unmarshal(revision.Signature.Payload, &signed); err != nil {
			t.Fatal(err)
		}
		if i > 0 && (signed.Nonce == previous.Nonce || !signed.IssuedAt.AsTime().After(previous.IssuedAt.AsTime())) {
			t.Fatal("heartbeat reused signed response")
		}
		previous = &signed
	}
	// The same revision's confirmation is a committed event, not a 20s poll.
	approved, err := privilegeddelivery.Confirm(id, saved.Revision, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || stream.Msg().UpdateSequence != approved.UpdateSequence {
		t.Fatalf("approval wake lost: %v", stream.Err())
	}
	now := time.Now()
	if err := dbcore.GetDBInstance().Model(&models.AgentCredential{}).Where("client = ?", id).Update("revoked_at", now).Error; err != nil {
		t.Fatal(err)
	}
	if stream.Receive() {
		t.Fatal("revoked stream kept authorizing")
	}
	if connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
		t.Fatalf("revocation outcome: %v", stream.Err())
	}
}

func TestSSHWindowWatchDistinguishesEnrollmentWithdrawalFromTokenRotation(t *testing.T) {
	for _, change := range []string{"revoke", "replace", "rotate"} {
		t.Run(change, func(t *testing.T) {
			id, token, _, handler := rescueWatchFixture(t)
			const enrollmentKey = "enrollment-before-change"
			db := dbcore.GetDBInstance()
			credentialRow := db.Model(&models.AgentCredential{}).Where("client = ?", id)
			require.NoError(t, credentialRow.Update("agent_key_id", enrollmentKey).Error)
			server := httptest.NewServer(handler)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client := configconnect.NewPrivilegedDeliveryServiceClient(server.Client(), server.URL)
			req := connect.NewRequest(&configv1.WatchPrivilegedDeliveryRequest{AgentId: id, ProtocolVersion: 2})
			req.Header().Set("Authorization", "Bearer "+token)
			req.Header().Set(rescueWatchHeader, "v1")
			req.Header().Set("X-Komari-Rescue-SSH-Window", "v1")
			stream, err := client.WatchPrivilegedDelivery(ctx, req)
			require.NoError(t, err)
			defer stream.Close()
			require.True(t, stream.Receive(), "opening heartbeat: %v", stream.Err())
			require.NotNil(t, stream.Msg().GetRevision())
			updates := map[string]any{"access_token_hash": "replacement-access-token-digest"}
			switch change {
			case "revoke":
				updates["revoked_at"] = time.Now()
			case "replace":
				updates["agent_key_id"] = "enrollment-after-change"
			}
			require.NoError(t, credentialRow.Updates(updates).Error)
			if change == "rotate" {
				require.False(t, stream.Receive())
				require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(stream.Err()))
				return
			}
			require.True(t, stream.Receive(), "signed withdrawal: %v", stream.Err())
			envelope := stream.Msg().GetRescueEnrollmentRevoked()
			require.NotNil(t, envelope)
			require.Len(t, envelope.Signatures, 1)
			signer, err := enrollment.ActiveSigningKey()
			require.NoError(t, err)
			require.Equal(t, signer.KeyID, envelope.Signatures[0].KeyId)
			require.True(t, ed25519.Verify(signer.PublicKey, envelope.Payload, envelope.Signatures[0].Value))
			var instruction securityv1.SignedInstruction
			require.NoError(t, proto.Unmarshal(envelope.Payload, &instruction))
			require.Equal(t, id, instruction.AgentId)
			require.Equal(t, "rescue:ssh-enrollment-revoked:v1", instruction.InstructionType)
			require.Equal(t, enrollmentKey, string(instruction.Body))
			require.False(t, stream.Receive())
		})
	}
}

// Actual backend + actual checked-in Nginx route, not a mocked heartbeat
// server. A short inherited proxy timeout and buffering expose missing route
// overrides. Runtime intentionally exceeds the former Agent's 30s deadline.
func TestRescueWatchNginxSurvivesThirtySecondsAndRevokes(t *testing.T) {
	if os.Getenv("KOMARI_NGINX_COMPONENT_TEST") != "1" {
		t.Skip("requires Nginx on a disposable CI runner")
	}
	nginx, err := exec.LookPath("nginx")
	if err != nil {
		t.Fatal(err)
	}
	id, token, saved, handler := rescueWatchFixture(t)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	endpoint := startWorkbenchNginx(t, nginx, strings.TrimPrefix(upstream.URL, "http://"),
		"proxy_read_timeout 500ms; proxy_buffering on; send_timeout 1s;")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	client := configconnect.NewPrivilegedDeliveryServiceClient(workbenchHTTP2Client(t), endpoint)
	req := connect.NewRequest(&configv1.WatchPrivilegedDeliveryRequest{AgentId: id, ProtocolVersion: 2})
	req.Header().Set("Authorization", "Bearer "+token)
	req.Header().Set(rescueWatchHeader, "v1")
	start := time.Now()
	stream, err := client.WatchPrivilegedDelivery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var previous time.Time
	count := 0
	for time.Since(start) < 32*time.Second {
		if !stream.Receive() {
			t.Fatalf("Nginx stream closed at %s: %v", time.Since(start), stream.Err())
		}
		message := stream.Msg().Revision
		if message.GetRevision() != saved.Revision || message.GetSignature() == nil {
			t.Fatal("missing signed revision")
		}
		var signed securityv1.SignedInstruction
		if err := proto.Unmarshal(message.Signature.Payload, &signed); err != nil {
			t.Fatal(err)
		}
		issued := signed.GetIssuedAt().AsTime()
		if !issued.After(previous) || time.Since(issued) > 2*time.Second {
			t.Fatal("heartbeat was replayed or buffered past the helper freshness window")
		}
		previous = issued
		count++
	}
	if count < 30 || requests.Load() != 1 || stream.ResponseHeader().Get(rescueWatchHeader) != "v1" {
		t.Fatalf("continuous watch evidence: frames=%d HTTP requests=%d", count, requests.Load())
	}
	if stream.ResponseHeader().Get("Cache-Control") != "no-store, no-transform" {
		t.Fatal("missing non-cacheable stream policy")
	}
	if err := dbcore.GetDBInstance().Model(&models.AgentCredential{}).Where("client = ?", id).Update("revoked_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if stream.Receive() || connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
		t.Fatalf("revoked credential continued authorizing: %v", stream.Err())
	}
	t.Logf("%d fresh signed frames on one request over %s, then live credential revocation through Nginx", count, time.Since(start))
}
