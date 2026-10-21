package imapclient

import (
	"testing"
	"time"
)

func TestValidateAddress(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{name: "host", addr: "imap.example.com:993"},
		{name: "ipv4", addr: "127.0.0.1:143"},
		{name: "ipv6", addr: "[2001:db8::1]:993"},
		{name: "empty", addr: "", wantErr: true},
		{name: "no port", addr: "imap.example.com", wantErr: true},
		{name: "empty host", addr: ":993", wantErr: true},
		{name: "zero port", addr: "imap.example.com:0", wantErr: true},
		{name: "port too large", addr: "imap.example.com:65536", wantErr: true},
		{name: "non numeric port", addr: "imap.example.com:imap", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAddress(tt.addr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAddress(%q) error = %v, wantErr %v", tt.addr, err, tt.wantErr)
			}
		})
	}
}

func TestValidateSecurity(t *testing.T) {
	for _, security := range []string{SecurityImplicitTLS, SecuritySTARTTLS, SecurityNone} {
		if err := validateSecurity(security); err != nil {
			t.Fatalf("validateSecurity(%q) = %v", security, err)
		}
	}
	for _, security := range []string{"", "tls", "ssl", "993"} {
		if err := validateSecurity(security); err == nil {
			t.Fatalf("validateSecurity(%q) = nil, want error", security)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	base := Config{
		Address:  "imap.example.com:993",
		Security: SecurityImplicitTLS,
		Username: "user@example.com",
		Password: "secret",
	}
	if err := validateConfig(base); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	withToken := base
	withToken.Password = ""
	withToken.AccessToken = "token"
	if err := validateConfig(withToken); err != nil {
		t.Fatalf("token config rejected: %v", err)
	}

	noCreds := base
	noCreds.Password = ""
	if err := validateConfig(noCreds); err == nil {
		t.Fatal("config without credentials accepted")
	}

	noUser := base
	noUser.Username = "  "
	if err := validateConfig(noUser); err == nil {
		t.Fatal("config without username accepted")
	}

	// Security must never be inferred from the port.
	insecure := base
	insecure.Security = ""
	if err := validateConfig(insecure); err == nil {
		t.Fatal("config without security accepted")
	}
}

func TestParseReferences(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "simple",
			raw:  "References: <a@example.com>\r\nSubject: x\r\n\r\nbody",
			want: []string{"a@example.com"},
		},
		{
			name: "multiple",
			raw:  "References: <a@example.com> <b@example.com>\r\n\r\nbody",
			want: []string{"a@example.com", "b@example.com"},
		},
		{
			name: "folded",
			raw:  "References: <a@example.com>\r\n\t<b@example.com>\r\n\r\nbody",
			want: []string{"a@example.com", "b@example.com"},
		},
		{
			name: "no brackets",
			raw:  "References: a@example.com, b@example.com\n\nbody",
			want: []string{"a@example.com", "b@example.com"},
		},
		{
			name: "missing header",
			raw:  "Subject: x\r\n\r\nbody",
			want: nil,
		},
		{
			name: "no blank line",
			raw:  "References: <a@example.com>",
			want: []string{"a@example.com"},
		},
		{
			name: "empty",
			raw:  "",
			want: nil,
		},
		{
			name: "case insensitive",
			raw:  "references: <a@example.com>\r\n\r\n",
			want: []string{"a@example.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseReferences([]byte(tt.raw))
			if len(got) != len(tt.want) {
				t.Fatalf("parseReferences() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("parseReferences() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestFolderName(t *testing.T) {
	tests := []struct {
		path, delim, want string
	}{
		{path: "INBOX", delim: "/", want: "INBOX"},
		{path: "Archive/2024", delim: "/", want: "2024"},
		{path: "Archive.2024", delim: ".", want: "2024"},
		{path: "Archive.2024", delim: "", want: "Archive.2024"},
		{path: "Archive/", delim: "/", want: "Archive/"},
	}
	for _, tt := range tests {
		if got := folderName(tt.path, tt.delim); got != tt.want {
			t.Fatalf("folderName(%q, %q) = %q, want %q", tt.path, tt.delim, got, tt.want)
		}
	}
}

func TestApplyHeaders(t *testing.T) {
	raw := []byte("From: =?utf-8?b?5aSc6L276YWx?= <alice@example.com>\r\n" +
		"To: bob@example.com,\r\n Carol <carol@example.com>\r\n" +
		"Subject: =?utf-8?q?hello_=E4=BD=A0=E5=A5=BD?=\r\n" +
		"Message-ID: <abc@example.com>\r\n" +
		"In-Reply-To: <parent@example.com>\r\n" +
		"References: <one@example.com>\r\n\t<two@example.com>\r\n" +
		"Date: Mon, 20 Oct 2025 10:00:00 +0000\r\n" +
		"\r\n")
	var env Envelope
	applyHeaders(&env, raw)
	if env.MessageID != "<abc@example.com>" {
		t.Fatalf("MessageID = %q", env.MessageID)
	}
	if env.InReplyTo != "<parent@example.com>" {
		t.Fatalf("InReplyTo = %q", env.InReplyTo)
	}
	if env.Subject != "hello 你好" {
		t.Fatalf("Subject = %q", env.Subject)
	}
	if len(env.From) != 1 || env.From[0].Name != "夜轻酱" || env.From[0].Email != "alice@example.com" {
		t.Fatalf("From = %+v", env.From)
	}
	if len(env.To) != 2 || env.To[1].Name != "Carol" || env.To[1].Email != "carol@example.com" {
		t.Fatalf("To = %+v", env.To)
	}
	if len(env.References) != 2 || env.References[0] != "one@example.com" || env.References[1] != "two@example.com" {
		t.Fatalf("References = %v", env.References)
	}
	if env.SentAt.UTC().Format(time.RFC3339) != "2025-10-20T10:00:00Z" {
		t.Fatalf("SentAt = %v", env.SentAt)
	}
}

func TestApplyHeadersToleratesGarbage(t *testing.T) {
	raw := []byte("From: not an address <>\r\n" +
		"To: broken ,, bob@example.com\r\n" +
		"Subject: plain\r\n" +
		"Date: not a date\r\n" +
		"nonsense line\r\n" +
		"\r\n")
	var env Envelope
	applyHeaders(&env, raw)
	if env.Subject != "plain" {
		t.Fatalf("Subject = %q", env.Subject)
	}
	if len(env.To) != 1 || env.To[0].Email != "bob@example.com" {
		t.Fatalf("To = %+v", env.To)
	}
	if !env.SentAt.IsZero() {
		t.Fatalf("SentAt = %v, want zero", env.SentAt)
	}
}

func TestValidateUIDs(t *testing.T) {
	if err := validateUIDs([]uint32{1, 2, 3}); err != nil {
		t.Fatalf("valid UIDs rejected: %v", err)
	}
	if err := validateUIDs([]uint32{0}); err == nil {
		t.Fatal("zero UID accepted")
	}
}
