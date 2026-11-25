package connectapi

import (
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/enrollment"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func signAgentControl(agentID, nonce string, deadline time.Time, message proto.Message) (*securityv1.SignedEnvelope, error) {
	key, err := enrollment.ActiveSigningKey()
	if err != nil {
		return nil, err
	}
	return signControlWithKey(key, agentID, nonce, deadline, message)
}

func signControlWithKey(key enrollment.SigningKey, agentID, nonce string, deadline time.Time, message proto.Message) (*securityv1.SignedEnvelope, error) {
	now := time.Now().UTC()
	if agentID == "" || nonce == "" || !deadline.After(now) {
		return nil, errors.New("signed control requires an agent, a nonce and an active lease")
	}
	encode := proto.MarshalOptions{Deterministic: true}
	body, err := encode.Marshal(message)
	if err != nil {
		return nil, err
	}
	payload, err := encode.Marshal(&securityv1.SignedInstruction{
		AgentId: agentID, Nonce: nonce, IssuedAt: timestamppb.New(now),
		ExpiresAt: timestamppb.New(deadline), InstructionType: string(message.ProtoReflect().Descriptor().FullName()), Body: body,
	})
	if err != nil {
		return nil, err
	}
	return key.Envelope(payload), nil
}
