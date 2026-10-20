// Package smtpclient submits messages with SMTP.
//
// Transport security is taken from Config.Security and never inferred from the
// port. XOAUTH2 is used when an access token is set, PLAIN otherwise. Unless
// Security is "none", the connection must be encrypted before any credentials
// or message data are sent.
package smtpclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
)

const (
	// SecurityImplicitTLS connects with TLS from the first byte.
	SecurityImplicitTLS = "implicit_tls"
	// SecuritySTARTTLS upgrades a plaintext connection with STARTTLS.
	SecuritySTARTTLS = "starttls"
	// SecurityNone connects without transport encryption.
	SecurityNone = "none"

	defaultTimeout  = 30 * time.Second
	defaultHELOHost = "localhost"
)

// Config describes how to connect to an SMTP submission server.
type Config struct {
	// Address is "host:port" or "[IPv6]:port". The port is mandatory.
	Address string
	// Security is one of implicit_tls, starttls or none. TLS is never
	// inferred from the port.
	Security string
	// Username is the authentication user name.
	Username string
	// Password is used for PLAIN authentication when AccessToken is empty.
	Password string
	// AccessToken, when set, selects XOAUTH2 authentication.
	AccessToken string
	// HELOHostname is sent in EHLO. Empty means "localhost".
	HELOHostname string
	// Timeout bounds dialing and each SMTP command. Zero means 30 seconds.
	Timeout time.Duration
}

// Send submits raw as a message from from to recipients.
//
// raw is normalized to CRLF line endings; dot stuffing is handled by the SMTP
// library. Once the server has accepted the message, errors from the final
// QUIT are not reported, because retrying would duplicate the message.
func Send(ctx context.Context, cfg Config, from string, recipients []string, raw []byte) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if err := validateAddressValue("from", from); err != nil {
		return err
	}
	if len(recipients) == 0 {
		return errors.New("smtpclient: at least one recipient is required")
	}
	for _, rcpt := range recipients {
		if err := validateAddressValue("recipient", rcpt); err != nil {
			return err
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, rawConn, err := dial(dialCtx, cfg, timeout)
	if err != nil {
		return err
	}
	defer c.Close()

	body := normalizeCRLF(raw)
	mailOpts := &smtp.MailOptions{Size: int64(len(body))}
	if needsUTF8(from, recipients) {
		mailOpts.UTF8 = true
	}

	err = do(ctx, rawConn, timeout, func() error {
		if err := c.Mail(from, mailOpts); err != nil {
			return fmt.Errorf("mail from %s: %w", from, err)
		}
		for _, rcpt := range recipients {
			if err := c.Rcpt(rcpt, nil); err != nil {
				return fmt.Errorf("rcpt to %s: %w", rcpt, err)
			}
		}
		w, err := c.Data()
		if err != nil {
			return fmt.Errorf("data: %w", err)
		}
		if _, err := w.Write(body); err != nil {
			_ = w.Close()
			return fmt.Errorf("write message: %w", err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("finish message: %w", err)
		}
		return nil
	})
	if err != nil {
		_ = c.Reset()
		return err
	}

	// The message is accepted at this point; a failing QUIT must not turn
	// into a retry.
	_ = c.Quit()
	return nil
}

// Test connects, authenticates and disconnects without sending a message.
func Test(ctx context.Context, cfg Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, rawConn, err := dial(dialCtx, cfg, timeout)
	if err != nil {
		return err
	}
	defer c.Close()

	if err := do(ctx, rawConn, timeout, func() error {
		return c.Noop()
	}); err != nil {
		return err
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("smtpclient: quit: %w", err)
	}
	return nil
}

// dial establishes the connection, switches on TLS as configured and
// authenticates. It also returns the connection used for deadline handling.
func dial(ctx context.Context, cfg Config, timeout time.Duration) (*smtp.Client, net.Conn, error) {
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, nil, fmt.Errorf("smtpclient: invalid address %q: %w", cfg.Address, err)
	}

	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return nil, nil, fmt.Errorf("smtpclient: dial %s: %w", cfg.Address, err)
	}
	// Bound the greeting, STARTTLS and authentication exchange by the
	// caller's context; individual commands later set their own deadlines.
	stop, watchDone := interruptOnDone(ctx, conn)
	defer func() {
		stop()
		<-watchDone
		_ = conn.SetDeadline(time.Time{})
	}()

	tlsConfig := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}

	var c *smtp.Client
	switch cfg.Security {
	case SecurityImplicitTLS:
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("smtpclient: TLS handshake with %s: %w", cfg.Address, err)
		}
		c = smtp.NewClient(tlsConn)
	case SecuritySTARTTLS:
		c, err = smtp.NewClientStartTLS(conn, tlsConfig)
		if err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("smtpclient: starttls with %s: %w", cfg.Address, err)
		}
	case SecurityNone:
		c = smtp.NewClient(conn)
	default:
		conn.Close()
		return nil, nil, fmt.Errorf("smtpclient: unknown security %q", cfg.Security)
	}

	c.CommandTimeout = timeout
	c.SubmissionTimeout = timeout

	if cfg.Security != SecurityNone {
		if _, ok := c.TLSConnectionState(); !ok {
			conn.Close()
			return nil, nil, errors.New("smtpclient: connection is not encrypted")
		}
	}

	helo := cfg.HELOHostname
	if helo == "" {
		helo = defaultHELOHost
	}
	if err := c.Hello(helo); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("smtpclient: hello %s: %w", helo, err)
	}

	if cfg.AccessToken != "" {
		if err := c.Auth(&xoauth2Client{username: cfg.Username, token: cfg.AccessToken}); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("smtpclient: xoauth2 authentication: %w", err)
		}
	} else if cfg.Username != "" {
		if err := c.Auth(sasl.NewPlainClient("", cfg.Username, cfg.Password)); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("smtpclient: plain authentication: %w", err)
		}
	}

	return c, conn, nil
}

// do applies a deadline and interrupts blocked I/O when ctx is canceled.
func do(ctx context.Context, conn net.Conn, timeout time.Duration, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	opCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	stop, watchDone := interruptOnDone(opCtx, conn)
	err := fn()
	stop()
	<-watchDone
	_ = conn.SetDeadline(time.Time{})

	if err == nil {
		return nil
	}
	if ctxErr := opCtx.Err(); ctxErr != nil {
		return fmt.Errorf("smtpclient: %w", ctxErr)
	}
	return fmt.Errorf("smtpclient: %w", err)
}

func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

func interruptOnDone(ctx context.Context, conn net.Conn) (stop func(), done <-chan struct{}) {
	stopCh := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stopCh:
		}
	}()
	return func() { close(stopCh) }, finished
}

// normalizeCRLF converts bare LF and bare CR to CRLF and guarantees a final
// CRLF. The input slice is returned unchanged when it is already normalized.
func normalizeCRLF(raw []byte) []byte {
	if isCRLFNormalized(raw) {
		return raw
	}
	out := make([]byte, 0, len(raw)+len(raw)/16+2)
	for i := 0; i < len(raw); {
		switch raw[i] {
		case '\r':
			out = append(out, '\r', '\n')
			if i+1 < len(raw) && raw[i+1] == '\n' {
				i += 2
			} else {
				i++
			}
		case '\n':
			out = append(out, '\r', '\n')
			i++
		default:
			out = append(out, raw[i])
			i++
		}
	}
	if len(out) > 0 && !(len(out) >= 2 && out[len(out)-2] == '\r' && out[len(out)-1] == '\n') {
		out = append(out, '\r', '\n')
	}
	return out
}

func isCRLFNormalized(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	for i := range raw {
		switch raw[i] {
		case '\n':
			if i == 0 || raw[i-1] != '\r' {
				return false
			}
		case '\r':
			if i+1 >= len(raw) || raw[i+1] != '\n' {
				return false
			}
		}
	}
	return raw[len(raw)-1] == '\n'
}

// needsUTF8 reports whether an SMTPUTF8-aware submission is required.
func needsUTF8(from string, recipients []string) bool {
	if !isASCII(from) {
		return true
	}
	for _, rcpt := range recipients {
		if !isASCII(rcpt) {
			return true
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func validateConfig(cfg Config) error {
	if err := validateAddress(cfg.Address); err != nil {
		return err
	}
	if err := validateSecurity(cfg.Security); err != nil {
		return err
	}
	if strings.ContainsAny(cfg.HELOHostname, "\r\n") {
		return errors.New("smtpclient: helo hostname must not contain line breaks")
	}
	if cfg.AccessToken == "" {
		if strings.TrimSpace(cfg.Username) == "" {
			return errors.New("smtpclient: username is required")
		}
		if strings.ContainsAny(cfg.Username, "\r\n") || strings.ContainsAny(cfg.Password, "\r\n") {
			return errors.New("smtpclient: credentials must not contain line breaks")
		}
	}
	return nil
}

func validateAddressValue(field, addr string) error {
	if strings.TrimSpace(addr) == "" {
		return fmt.Errorf("smtpclient: %s address is required", field)
	}
	if strings.ContainsAny(addr, "\r\n") {
		return fmt.Errorf("smtpclient: %s address must not contain line breaks", field)
	}
	return nil
}

// validateAddress checks a "host:port" or "[IPv6]:port" address.
func validateAddress(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("smtpclient: address is required")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("smtpclient: invalid address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("smtpclient: invalid address %q: missing host", addr)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("smtpclient: invalid address %q: port must be 1-65535", addr)
	}
	return nil
}

func validateSecurity(security string) error {
	switch security {
	case SecurityImplicitTLS, SecuritySTARTTLS, SecurityNone:
		return nil
	case "":
		return errors.New("smtpclient: security must be implicit_tls, starttls or none")
	default:
		return fmt.Errorf("smtpclient: unknown security %q", security)
	}
}
