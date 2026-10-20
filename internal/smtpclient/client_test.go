package smtpclient

import (
	"bytes"
	"testing"
)

func TestValidateAddress(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{addr: "smtp.example.com:587"},
		{addr: "127.0.0.1:25"},
		{addr: "[2001:db8::1]:465"},
		{addr: "", wantErr: true},
		{addr: "smtp.example.com", wantErr: true},
		{addr: ":587", wantErr: true},
		{addr: "smtp.example.com:0", wantErr: true},
		{addr: "smtp.example.com:65536", wantErr: true},
		{addr: "smtp.example.com:smtp", wantErr: true},
	}
	for _, tt := range tests {
		err := validateAddress(tt.addr)
		if (err != nil) != tt.wantErr {
			t.Fatalf("validateAddress(%q) error = %v, wantErr %v", tt.addr, err, tt.wantErr)
		}
	}
}

func TestValidateSecurity(t *testing.T) {
	for _, security := range []string{SecurityImplicitTLS, SecuritySTARTTLS, SecurityNone} {
		if err := validateSecurity(security); err != nil {
			t.Fatalf("validateSecurity(%q) = %v", security, err)
		}
	}
	for _, security := range []string{"", "ssl", "587"} {
		if err := validateSecurity(security); err == nil {
			t.Fatalf("validateSecurity(%q) = nil, want error", security)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	base := Config{
		Address:      "smtp.example.com:465",
		Security:     SecurityImplicitTLS,
		Username:     "user@example.com",
		Password:     "secret",
		HELOHostname: "mail.example.com",
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

	noUser := base
	noUser.Username = " "
	if err := validateConfig(noUser); err == nil {
		t.Fatal("config without username accepted")
	}

	badHELO := base
	badHELO.HELOHostname = "mail.example.com\r\nMAIL FROM:<evil@example.com>"
	if err := validateConfig(badHELO); err == nil {
		t.Fatal("config with CRLF in helo hostname accepted")
	}

	inferred := base
	inferred.Security = ""
	if err := validateConfig(inferred); err == nil {
		t.Fatal("config without security accepted")
	}
}

func TestValidateAddressValue(t *testing.T) {
	if err := validateAddressValue("from", "user@example.com"); err != nil {
		t.Fatalf("valid address rejected: %v", err)
	}
	if err := validateAddressValue("from", "  "); err == nil {
		t.Fatal("empty address accepted")
	}
	if err := validateAddressValue("from", "a@example.com\r\nRCPT TO:<b@example.com>"); err == nil {
		t.Fatal("CRLF address accepted")
	}
}

func TestNormalizeCRLF(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "already normalized", in: "a\r\nb\r\n", want: "a\r\nb\r\n"},
		{name: "lf only", in: "a\nb\n", want: "a\r\nb\r\n"},
		{name: "mixed", in: "a\r\nb\nc\r\n", want: "a\r\nb\r\nc\r\n"},
		{name: "cr only", in: "a\rb\r", want: "a\r\nb\r\n"},
		{name: "missing final crlf", in: "a\r\nb", want: "a\r\nb\r\n"},
		{name: "crlfcrlf", in: "a\r\n\r\n", want: "a\r\n\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCRLF([]byte(tt.in))
			if string(got) != tt.want {
				t.Fatalf("normalizeCRLF(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeCRLFDoesNotMutateInput(t *testing.T) {
	in := []byte("a\nb\n")
	_ = normalizeCRLF(in)
	if !bytes.Equal(in, []byte("a\nb\n")) {
		t.Fatalf("input mutated: %q", in)
	}
}

func TestNeedsUTF8(t *testing.T) {
	if needsUTF8("user@example.com", []string{"a@example.com"}) {
		t.Fatal("ASCII envelope reported as UTF-8")
	}
	if !needsUTF8("üser@example.com", []string{"a@example.com"}) {
		t.Fatal("non-ASCII sender not detected")
	}
	if !needsUTF8("user@example.com", []string{"a@example.com", "b@exämple.com"}) {
		t.Fatal("non-ASCII recipient not detected")
	}
}
