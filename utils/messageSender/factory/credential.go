package factory

import (
	"context"
	"errors"
	"net/mail"
	"strconv"
	"strings"
)

// CredentialDelivery is deliberately separate from alert events and recipients.
// Implementations must send only to Address, without BCC/group/thread fallback.
type CredentialDelivery struct {
	Kind    string
	Address string
	Title   string
	Message string
}

type CredentialSender interface {
	SupportsCredentialKind(string) bool
	SendCredential(context.Context, CredentialDelivery) error
}

var ErrCredentialDelivery = errors.New("private credential delivery is unavailable")

func NormalizeCredentialAddress(kind, address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" || len(address) > 320 || strings.ContainsAny(address, "\r\n\x00") {
		return "", ErrCredentialDelivery
	}
	switch kind {
	case "email":
		for _, c := range address {
			if c > 127 {
				return "", ErrCredentialDelivery
			}
		}
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Name != "" || parsed.Address != address {
			return "", ErrCredentialDelivery
		}
		at := strings.LastIndexByte(address, '@')
		if at <= 0 || at == len(address)-1 {
			return "", ErrCredentialDelivery
		}
		return address[:at+1] + strings.ToLower(address[at+1:]), nil
	case "telegram":
		id, err := strconv.ParseInt(address, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != address {
			return "", ErrCredentialDelivery
		}
		return address, nil
	default:
		return "", ErrCredentialDelivery
	}
}
