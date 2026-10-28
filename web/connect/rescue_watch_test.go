package connectapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/pkg/rpc"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	configconnect "github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"google.golang.org/protobuf/proto"
)

func TestRescueWatchFreshSignedHeartbeatAndCredentialRevocation(t *testing.T) {
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
	path, handler := configconnect.NewPrivilegedDeliveryServiceHandler(&privilegedDeliveryService{})
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(rpc.NewContextWithMeta(r.Context(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(id)})))
	}))
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
	var previous securityv1.SignedInstruction
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
		previous = signed
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
	if err := db.Model(&models.AgentCredential{}).Where("client = ?", id).Update("revoked_at", now).Error; err != nil {
		t.Fatal(err)
	}
	if stream.Receive() {
		t.Fatal("revoked stream kept authorizing")
	}
	if connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
		t.Fatalf("revocation outcome: %v", stream.Err())
	}
}
