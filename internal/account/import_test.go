package account

import (
	"testing"

	"github.com/mailquay/mailquay/internal/emclient"
)

func TestBuildImportPreview(t *testing.T) {
	accounts := []emclient.Account{
		{Email: "ready@example.com", AuthType: "password", Password: "secret", ReceiveProtocol: "imap", IMAPAddr: "imap.example.com:993", SMTPAddr: "smtp.example.com:587"},
		{Email: "duplicate@example.com", AuthType: "password", Password: "secret", ReceiveProtocol: "imap"},
		{Email: "oauth@example.com", AuthType: "oauth2", ReceiveProtocol: "imap"},
		{Email: "nopass@example.com", AuthType: "password", SkipCode: "no_password", ReceiveProtocol: "imap"},
		{Email: "READY@example.com", AuthType: "password", SkipCode: "no_password", ReceiveProtocol: "imap"},
	}
	candidates, session := buildImportPreview(accounts, map[string]bool{"duplicate@example.com": true})

	if len(candidates) != 4 {
		t.Fatalf("got %d candidates, want 4", len(candidates))
	}
	byEmail := make(map[string]ImportAccount)
	for _, candidate := range candidates {
		byEmail[candidate.Email] = candidate
	}

	ready := byEmail["ready@example.com"]
	if !ready.Importable || !ready.Selected || ready.Reason != "" {
		t.Errorf("ready account = %+v", ready)
	}
	if byEmail["duplicate@example.com"].Reason != "duplicate" {
		t.Errorf("duplicate reason = %q", byEmail["duplicate@example.com"].Reason)
	}
	if byEmail["oauth@example.com"].Reason != "oauth2" {
		t.Errorf("oauth reason = %q", byEmail["oauth@example.com"].Reason)
	}
	if byEmail["nopass@example.com"].Reason != "no_password" {
		t.Errorf("missing password reason = %q", byEmail["nopass@example.com"].Reason)
	}
	if !byEmail["ready@example.com"].Importable {
		t.Error("the importable duplicate of an email should be preferred")
	}
	if len(session) != 1 || session["ready@example.com"].Password != "secret" {
		t.Errorf("session accounts = %+v", session)
	}
}
