package pop3client

import (
	"context"
	"testing"
	"time"
)

func TestValidateAddress(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{addr: "pop.example.com:995"},
		{addr: "127.0.0.1:110"},
		{addr: "[::1]:995"},
		{addr: "", wantErr: true},
		{addr: "pop.example.com", wantErr: true},
		{addr: ":995", wantErr: true},
		{addr: "pop.example.com:0", wantErr: true},
		{addr: "pop.example.com:65536", wantErr: true},
		{addr: "pop.example.com:pop3", wantErr: true},
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
	for _, security := range []string{"", "ssl", "110"} {
		if err := validateSecurity(security); err == nil {
			t.Fatalf("validateSecurity(%q) = nil, want error", security)
		}
	}
}

func TestValidateConfig(t *testing.T) {
	base := Config{
		Address:  "pop.example.com:995",
		Security: SecurityImplicitTLS,
		Username: "user@example.com",
		Password: "secret",
	}
	if err := validateConfig(base); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	noUser := base
	noUser.Username = " "
	if err := validateConfig(noUser); err == nil {
		t.Fatal("config without username accepted")
	}

	injected := base
	injected.Password = "secret\r\nDELE 1"
	if err := validateConfig(injected); err == nil {
		t.Fatal("config with CRLF in password accepted")
	}

	inferred := base
	inferred.Security = ""
	if err := validateConfig(inferred); err == nil {
		t.Fatal("config without security accepted")
	}
}

func TestParseStatResponse(t *testing.T) {
	count, size, err := parseStatResponse("3 1024")
	if err != nil {
		t.Fatalf("parseStatResponse: %v", err)
	}
	if count != 3 || size != 1024 {
		t.Fatalf("got (%d, %d), want (3, 1024)", count, size)
	}

	for _, text := range []string{"", "3", "x 4", "3 y", "-1 4", "3 -1"} {
		if _, _, err := parseStatResponse(text); err == nil {
			t.Fatalf("parseStatResponse(%q) = nil error", text)
		}
	}
}

func TestParseUIDLLines(t *testing.T) {
	msgs, err := parseUIDLLines([]string{"2 uid-b", "1 uid-a", ""})
	if err != nil {
		t.Fatalf("parseUIDLLines: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Number != 1 || msgs[0].UIDL != "uid-a" || msgs[1].Number != 2 {
		t.Fatalf("unexpected messages: %+v", msgs)
	}

	if _, err := parseUIDLLines([]string{"1"}); err == nil {
		t.Fatal("malformed UIDL row accepted")
	}
	if _, err := parseUIDLLines([]string{"0 uid"}); err == nil {
		t.Fatal("zero message number accepted")
	}
}

func TestParseLISTLines(t *testing.T) {
	sizes, err := parseLISTLines([]string{"1 100", "2 2500", ""})
	if err != nil {
		t.Fatalf("parseLISTLines: %v", err)
	}
	if sizes[1] != 100 || sizes[2] != 2500 {
		t.Fatalf("unexpected sizes: %+v", sizes)
	}
	if _, err := parseLISTLines([]string{"1 x"}); err == nil {
		t.Fatal("malformed LIST row accepted")
	}
}

func TestEncodeLines(t *testing.T) {
	if got := encodeLines(nil); len(got) != 0 {
		t.Fatalf("encodeLines(nil) = %q", got)
	}
	got := string(encodeLines([]string{"a", "b"}))
	if got != "a\r\nb\r\n" {
		t.Fatalf("encodeLines() = %q", got)
	}
}

func TestOperationDeadline(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if got := operationDeadline(ctx, time.Hour); !got.Equal(deadline) {
		t.Fatalf("operationDeadline() = %v, want caller deadline %v", got, deadline)
	}
}
