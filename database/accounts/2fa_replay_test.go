package accounts

import (
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// A TOTP code must be pinned to the step it was minted for, not to the step the
// server happens to be in when it arrives. Validation accepts one step of skew
// either way, so recording "now" would leave a code minted for step N spendable
// again once the clock reads N+1 - the replay this guards against.
func TestTOTPCodeCounterPinsTheCodeToItsOwnStep(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: "user"})
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	secret := key.Secret()

	minted := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	code, err := totp.GenerateCode(secret, minted)
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	want := minted.Unix() / int64(totpPeriod.Seconds())

	// The same code presented across the whole window it is accepted in must
	// always resolve to the step it was minted for.
	for _, offset := range []time.Duration{0, totpPeriod, -totpPeriod} {
		at := minted.Add(offset)
		got, ok := totpCodeCounter(code, secret, at)
		if !ok {
			t.Fatalf("code rejected at offset %s, but validation accepts this skew", offset)
		}
		if got != want {
			t.Fatalf("offset %s resolved to counter %d, want %d: a replay would get a fresh counter",
				offset, got, want)
		}
	}

	// Well outside the window it must not resolve at all.
	if _, ok := totpCodeCounter(code, secret, minted.Add(10*totpPeriod)); ok {
		t.Fatal("a long-expired code must not resolve to any counter")
	}
	if _, ok := totpCodeCounter("000000", secret, minted); ok {
		// 000000 is a valid-looking but almost certainly wrong code.
		t.Skip("the placeholder code happened to be valid for this secret")
	}
}

// The hardcoded options must match what totp.Validate uses, otherwise a code
// this fork issues could resolve under one set of parameters and validate under
// another.
func TestTOTPCodeCounterMatchesValidateParameters(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "test", AccountName: "user"})
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	secret := key.Secret()
	now := time.Now().UTC()
	code, err := totp.GenerateCode(secret, now)
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}

	if !totp.Validate(code, secret) {
		t.Fatal("fixture code must be valid for totp.Validate")
	}
	if _, ok := totpCodeCounter(code, secret, now); !ok {
		t.Fatal("a code accepted by totp.Validate must resolve to a counter")
	}

	// Guard the assumption directly: these are the parameters Validate applies.
	ok, err := totp.ValidateCustom(code, secret, now, totp.ValidateOpts{
		Period: uint(totpPeriod.Seconds()), Skew: 1,
		Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil || !ok {
		t.Fatalf("parameter assumption broken: ok=%v err=%v", ok, err)
	}
}
