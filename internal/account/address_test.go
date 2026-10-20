package account

import "testing"

func TestNormalizeAddress(t *testing.T) {
	valid := map[string]string{
		"imap.example.com:993": "imap.example.com:993",
		"127.0.0.1:143":        "127.0.0.1:143",
		"[2001:db8::1]:993":    "[2001:db8::1]:993",
	}
	for in, want := range valid {
		got, err := NormalizeAddress(in)
		if err != nil {
			t.Fatalf("NormalizeAddress(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("NormalizeAddress(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "imap.example.com", "2001:db8::1:993", "host:0", "host:70000", "[bad]:993"} {
		if _, err := NormalizeAddress(in); err == nil {
			t.Errorf("NormalizeAddress(%q) unexpectedly succeeded", in)
		}
	}
}

func TestValidatePort(t *testing.T) {
	if err := ValidatePort("host:65535"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidatePort("host:0"); err == nil {
		t.Fatal("expected port rejection")
	}
}
