package discovery

import (
	"errors"
	"testing"
)

func TestDiscoverRegistry(t *testing.T) {
	result, err := Discover(t.Context(), "someone@gmail.com")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if result.Source != SourceRegistry || result.Provider != "gmail" || result.OAuthProvider != "google" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.IMAP == nil || result.IMAP.Addr != "imap.gmail.com:993" || result.IMAP.Security != securityImplicitTLS {
		t.Fatalf("unexpected IMAP endpoint: %+v", result.IMAP)
	}
	if result.SMTP == nil || result.SMTP.Addr != "smtp.gmail.com:465" {
		t.Fatalf("unexpected SMTP endpoint: %+v", result.SMTP)
	}
	if result.ReceiveProtocol != "imap" {
		t.Fatalf("unexpected protocol: %q", result.ReceiveProtocol)
	}
}

func TestDiscoverInvalidEmail(t *testing.T) {
	for _, address := range []string{"", "not-an-email", "user@localhost", "user@127.0.0.1"} {
		if _, err := Discover(t.Context(), address); !errors.Is(err, ErrInvalidEmail) {
			t.Fatalf("expected ErrInvalidEmail for %q, got %v", address, err)
		}
	}
}

func TestDiscoverMicrosoftRegistry(t *testing.T) {
	result, err := Discover(t.Context(), "someone@outlook.com")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if result.OAuthProvider != "microsoft" || result.SMTP == nil || result.SMTP.Security != securitySTARTTLS {
		t.Fatalf("unexpected result: %+v", result)
	}
}
