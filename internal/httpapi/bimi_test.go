package httpapi

import (
	"net"
	"testing"
)

func TestParseBIMIRecord(t *testing.T) {
	if got := parseBIMIRecord("v=BIMI1; l=https://example.com/logo.svg; a=https://example.com/vmc.pem"); got != "https://example.com/logo.svg" {
		t.Errorf("logo = %q", got)
	}
	if got := parseBIMIRecord("v=BIMI1; l=https://example.com/logo.svg"); got != "https://example.com/logo.svg" {
		t.Errorf("logo = %q", got)
	}
	if got := parseBIMIRecord("v=BIMI2; l=https://example.com/logo.svg"); got != "" {
		t.Errorf("version 2 logo = %q", got)
	}
	if got := parseBIMIRecord("l=https://example.com/logo.svg"); got != "" {
		t.Errorf("missing version logo = %q", got)
	}
}

func TestValidBIMISVG(t *testing.T) {
	cases := []struct {
		name string
		svg  string
		ok   bool
	}{
		{name: "minimal", svg: `<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0h1v1z"/></svg>`, ok: true},
		{name: "data image", svg: `<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/png;base64,AA=="/></svg>`, ok: true},
		{name: "fragment", svg: `<svg xmlns="http://www.w3.org/2000/svg"><use href="#mark"/></svg>`, ok: true},
		{name: "script", svg: `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`, ok: false},
		{name: "event", svg: `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`, ok: false},
		{name: "external href", svg: `<svg xmlns="http://www.w3.org/2000/svg"><image href="https://evil.example/x.png"/></svg>`, ok: false},
		{name: "external style", svg: `<svg xmlns="http://www.w3.org/2000/svg"><style>@import url("https://evil.example/x.css");</style></svg>`, ok: false},
		{name: "quoted external style", svg: `<svg xmlns="http://www.w3.org/2000/svg"><style>.x{background:url("https://evil.example/x")}</style></svg>`, ok: false},
		{name: "nested svg data", svg: `<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/svg+xml;base64,AA=="/></svg>`, ok: false},
		{name: "doctype", svg: `<!DOCTYPE svg [<!ENTITY x "y">]><svg xmlns="http://www.w3.org/2000/svg"/>`, ok: false},
		{name: "not svg", svg: `<html/>`, ok: false},
		{name: "malformed", svg: `<svg`, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validBIMISVG([]byte(tc.svg)); got != tc.ok {
				t.Errorf("validBIMISVG = %v, want %v", got, tc.ok)
			}
		})
	}
}

func TestBIMIBlockedIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{ip: "127.0.0.1", blocked: true},
		{ip: "10.0.0.1", blocked: true},
		{ip: "172.16.0.1", blocked: true},
		{ip: "192.168.1.1", blocked: true},
		{ip: "169.254.169.254", blocked: true},
		{ip: "100.64.0.1", blocked: true},
		{ip: "0.0.0.0", blocked: true},
		{ip: "::1", blocked: true},
		{ip: "fc00::1", blocked: true},
		{ip: "fe80::1", blocked: true},
		{ip: "8.8.8.8", blocked: false},
		{ip: "198.18.0.1", blocked: false},
		{ip: "2001:2::1", blocked: false},
	}
	for _, tc := range cases {
		if got := bimiBlockedIP(net.ParseIP(tc.ip)); got != tc.blocked {
			t.Errorf("%s: blocked = %v, want %v", tc.ip, got, tc.blocked)
		}
	}
}
