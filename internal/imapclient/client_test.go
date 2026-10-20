package imapclient

import (
	"testing"

	"github.com/emersion/go-imap/v2"
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

func TestConvertAddresses(t *testing.T) {
	if got := convertAddresses(nil); got != nil {
		t.Fatalf("convertAddresses(nil) = %v, want nil", got)
	}
	in := []imap.Address{
		{Name: "Alice", Mailbox: "alice", Host: "example.com"},
		{Mailbox: "bob@example.com"},
		{},
	}
	got := convertAddresses(in)
	if len(got) != 2 {
		t.Fatalf("convertAddresses() = %v, want 2 entries", got)
	}
	if got[0].Name != "Alice" || got[0].Email != "alice@example.com" {
		t.Fatalf("unexpected first address: %+v", got[0])
	}
	if got[1].Email != "bob@example.com" {
		t.Fatalf("unexpected second address: %+v", got[1])
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
