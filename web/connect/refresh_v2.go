package connectapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/models"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
)

type refreshV2Request struct {
	Protocol     int     `json:"protocol"`
	AgentID      string  `json:"agent_id"`
	RefreshToken string  `json:"refresh_token"`
	RequestID    string  `json:"request_id"`
	Generation   *uint64 `json:"generation"`
	Timestamp    string  `json:"timestamp"`
	Nonce        string  `json:"nonce"`
}

type refreshV2Response struct {
	Protocol          int    `json:"protocol"`
	AgentID           string `json:"agent_id"`
	RequestID         string `json:"request_id"`
	Generation        uint64 `json:"generation"`
	CredentialsSHA256 string `json:"credentials_sha256"`
}

// Explicit version markers never fall through to permissive legacy parsing.
func refreshProtocolVersion(envelope *securityv1.SignedEnvelope) (int, error) {
	if envelope == nil {
		return 1, nil
	}
	var header map[string]json.RawMessage
	if json.Unmarshal(envelope.Payload, &header) != nil {
		return 1, nil
	}
	raw, present := header["protocol"]
	if !present {
		return 1, nil
	}
	var version int
	if json.Unmarshal(raw, &version) != nil || version != 2 {
		return 0, errors.New("unsupported refresh version")
	}
	return 2, nil
}

func verifyRefreshV2(envelope *securityv1.SignedEnvelope, record models.AgentCredential, token string) (string, *uint64, error) {
	denied := errors.New("invalid versioned refresh proof")
	if envelope == nil || len(envelope.Payload) > 4096 {
		return "", nil, denied
	}
	key, err := base64.StdEncoding.DecodeString(record.AgentPublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return "", nil, denied
	}
	var signature []byte
	for _, sig := range envelope.Signatures {
		if sig.Algorithm == securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519 && ed25519.Verify(key, envelope.Payload, sig.Value) {
			signature = sig.Value
			break
		}
	}
	if signature == nil {
		return "", nil, denied
	}
	var proof refreshV2Request
	decoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&proof) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return "", nil, denied
	}
	at, err := time.Parse(time.RFC3339Nano, proof.Timestamp)
	if err != nil || time.Since(at) > 2*time.Minute || time.Until(at) > 30*time.Second {
		return "", nil, denied
	}
	if proof.Protocol != 2 || proof.AgentID != record.Client || proof.RefreshToken != token {
		return "", nil, denied
	}
	if _, err := uuid.Parse(proof.RequestID); err != nil {
		return "", nil, denied
	}
	if _, err := uuid.Parse(proof.Nonce); err != nil {
		return "", nil, denied
	}
	digest := sha256.Sum256(signature)
	if !recordAndVerifyNonce("refresh-v2:"+record.Client+":"+hex.EncodeToString(digest[:]), 2*time.Minute) {
		return "", nil, denied
	}
	return proof.RequestID, proof.Generation, nil
}
