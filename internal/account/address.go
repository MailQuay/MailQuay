package account

import (
	"fmt"
	"net"
	"net/mail"
	"strconv"
	"strings"
)

// NormalizeAddress validates "host:port" or "[ipv6]:port" and returns a
// canonical form. Ports are always required.
func NormalizeAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("address is required")
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("address must be host:port: %w", err)
	}
	if host == "" {
		return "", fmt.Errorf("address host is empty")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("port must be between 1 and 65535")
	}
	if strings.HasPrefix(raw, "[") {
		if net.ParseIP(host) == nil {
			return "", fmt.Errorf("invalid IPv6 address %q", host)
		}
		return "[" + host + "]:" + port, nil
	}
	if strings.Contains(host, ":") {
		if !strings.HasPrefix(raw, "[") {
			return "", fmt.Errorf("IPv6 addresses must use [address]:port")
		}
		if net.ParseIP(host) == nil {
			return "", fmt.Errorf("invalid IPv6 address %q", host)
		}
		return "[" + host + "]:" + port, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return net.JoinHostPort(host, port), nil
	}
	if strings.ContainsAny(host, " /\\") {
		return "", fmt.Errorf("invalid host %q", host)
	}
	return net.JoinHostPort(host, port), nil
}

// ValidatePort checks the port range for a host:port address.
func ValidatePort(address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

// NormalizeEmail parses and canonicalizes an email address.
func NormalizeEmail(raw string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid email address: %w", err)
	}
	return strings.ToLower(addr.Address), nil
}

// SplitEmail returns the display name and address when present.
func SplitEmail(raw string) (string, string) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return "", strings.TrimSpace(raw)
	}
	return addr.Name, addr.Address
}
