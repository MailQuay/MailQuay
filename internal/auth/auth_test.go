package auth

import (
	"net/http/httptest"
	"testing"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct horse battery", hash) {
		t.Fatal("expected password to verify")
	}
	if VerifyPassword("wrong password", hash) {
		t.Fatal("wrong password verified")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected short password rejection")
	}
}

func TestHostExempt(t *testing.T) {
	cases := []struct {
		host  string
		extra []string
		want  bool
	}{
		{"localhost", nil, true},
		{"mail.localhost", nil, true},
		{"app.lan", nil, true},
		{"box.home.arpa", nil, true},
		{"127.0.0.1", nil, true},
		{"10.1.2.3", nil, true},
		{"172.20.0.4", nil, true},
		{"192.168.1.7", nil, true},
		{"169.254.1.1", nil, true},
		{"::1", nil, true},
		{"fd00::5", nil, true},
		{"8.8.8.8", nil, false},
		{"mail.example.com", nil, false},
		{"mail.example.com", []string{".example.com"}, true},
		{"example.com", []string{"example.com"}, true},
		{"192.0.2.10", []string{"192.0.2.0/24"}, true},
		{"198.51.100.9", []string{"192.0.2.0/24"}, false},
	}
	for _, tc := range cases {
		if got := HostExempt(tc.host, tc.extra); got != tc.want {
			t.Errorf("HostExempt(%q, %v) = %v, want %v", tc.host, tc.extra, got, tc.want)
		}
	}
}

func TestValidCSRF(t *testing.T) {
	if !ValidCSRF("abc", "abc") {
		t.Fatal("matching tokens rejected")
	}
	if ValidCSRF("abc", "abd") || ValidCSRF("", "") || ValidCSRF("abc", "") {
		t.Fatal("mismatched tokens accepted")
	}
}

func TestInsecureTransport(t *testing.T) {
	allow := []string{"mail.example.net"}
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Host = "example.com"
	req.RemoteAddr = "203.0.113.5:1234"
	if !InsecureTransport(req, nil, allow) {
		t.Fatal("public HTTP host should warn")
	}
	req.Host = "mail.example.net"
	if InsecureTransport(req, nil, allow) {
		t.Fatal("explicitly allowed host should not warn")
	}
	req.Host = "example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	if !InsecureTransport(req, nil, allow) {
		t.Fatal("untrusted proxy header must be ignored")
	}
	if InsecureTransport(req, []string{"203.0.113.0/24"}, allow) {
		t.Fatal("trusted proxy HTTPS header should be honored")
	}
}
