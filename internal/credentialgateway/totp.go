package credentialgateway

import (
	"context"
	"encoding/json"

	"github.com/komari-monitor/komari/database/accounts"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
)

type totpProvider struct{}

func (totpProvider) Descriptor() credential.Descriptor {
	return credential.Descriptor{ID: "native:totp", DisplayName: "TOTP", Kind: "legacy_totp", Purposes: []credential.Purpose{credential.BindContact, credential.ManageCredentials, credential.UnlockWorkspace, credential.LoginSettings, credential.SensitiveOperation}}
}

func (totpProvider) Available(ctx context.Context, binding credential.Binding) (bool, error) {
	b, user, err := identity(ctx)
	if err != nil || b != binding {
		return false, credential.ErrUnauthenticated
	}
	return user.TwoFactor != "", nil
}

func (totpProvider) Begin(ctx context.Context, attempt credential.Attempt) (credential.Prompt, credential.VerifyFunc, error) {
	b, user, err := identity(ctx)
	if err != nil || b != attempt.Binding || user.TwoFactor == "" {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	// No expected code/secret is stored in the challenge or public prompt.
	return credential.Prompt{Type: "otp", Data: json.RawMessage(`{"digits":6,"method":"totp"}`)}, func(ctx context.Context, raw json.RawMessage) error {
		if err := validateBinding(ctx, attempt.Binding); err != nil {
			return err
		}
		var proof struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &proof) != nil || len(proof.Code) != 6 {
			return credential.ErrVerificationFailed
		}
		for _, c := range proof.Code {
			if c < '0' || c > '9' {
				return credential.ErrVerificationFailed
			}
		}
		// Keep the existing database-backed atomic replay counter. Both old and
		// new paths consume the same counter, so codes cannot cross the adapters.
		ok, err := accounts.Verify2Fa(attempt.Binding.UserID, proof.Code)
		if err != nil {
			return credential.ErrProviderUnavailable
		}
		if !ok {
			return credential.ErrVerificationFailed
		}
		return nil
	}, nil
}
