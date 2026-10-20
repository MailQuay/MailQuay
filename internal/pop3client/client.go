// Package pop3client implements the POP3 commands MailQuay needs: STAT, UIDL,
// TOP, RETR, DELE and QUIT.
//
// It speaks the protocol directly instead of using a third-party client so
// that implicit TLS, STARTTLS and per-operation deadlines behave the same way
// as in the IMAP client. Context cancellation interrupts blocked reads.
package pop3client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SecurityImplicitTLS connects with TLS from the first byte.
	SecurityImplicitTLS = "implicit_tls"
	// SecuritySTARTTLS upgrades a plaintext connection with STLS.
	SecuritySTARTTLS = "starttls"
	// SecurityNone connects without transport encryption.
	SecurityNone = "none"

	defaultTimeout = 30 * time.Second
)

// Config describes how to connect to a POP3 server.
type Config struct {
	// Address is "host:port" or "[IPv6]:port". The port is mandatory.
	Address string
	// Security is one of implicit_tls, starttls or none. TLS is never
	// inferred from the port.
	Security string
	// Username is the POP3 user name.
	Username string
	// Password is sent with PASS.
	Password string
	// Timeout bounds each operation. Zero means 30 seconds.
	Timeout time.Duration
}

// MessageInfo is one row of the UIDL listing.
type MessageInfo struct {
	Number int
	UIDL   string
	Size   int64
}

// Client is an authenticated POP3 session.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	timeout time.Duration

	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// Dial connects, negotiates transport security and authenticates.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	host, _, _ := net.SplitHostPort(cfg.Address)

	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("pop3client: dial %s: %w", cfg.Address, err)
	}

	if cfg.Security == SecurityImplicitTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
		hctx, cancel := withTimeout(ctx, timeout)
		err := tlsConn.HandshakeContext(hctx)
		cancel()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("pop3client: TLS handshake with %s: %w", cfg.Address, err)
		}
		conn = tlsConn
	}

	c := &Client{
		conn:    conn,
		r:       bufio.NewReader(conn),
		w:       bufio.NewWriter(conn),
		timeout: timeout,
	}

	if err := c.run(ctx, func() error {
		if _, err := c.readStatusLine("greeting"); err != nil {
			return err
		}
		if cfg.Security == SecuritySTARTTLS {
			if _, _, err := c.exchange("STLS", "STLS", false); err != nil {
				return err
			}
			if err := c.upgradeTLS(ctx, host, timeout); err != nil {
				return err
			}
		}
		if _, _, err := c.exchange("USER", "USER "+cfg.Username, false); err != nil {
			return err
		}
		if _, _, err := c.exchange("PASS", "PASS "+cfg.Password, false); err != nil {
			return err
		}
		return nil
	}); err != nil {
		c.closeConn()
		return nil, err
	}

	return c, nil
}

// upgradeTLS turns the current connection into a TLS connection after a
// successful STLS. Any bytes already buffered by the reader are replayed to
// the TLS layer first.
func (c *Client) upgradeTLS(ctx context.Context, host string, timeout time.Duration) error {
	var base net.Conn = c.conn
	if n := c.r.Buffered(); n > 0 {
		buffered, _ := c.r.Peek(n)
		prefix := append([]byte(nil), buffered...)
		base = &prefixConn{Conn: c.conn, r: io.MultiReader(bytes.NewReader(prefix), c.conn)}
	}
	tlsConn := tls.Client(base, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	hctx, cancel := withTimeout(ctx, timeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hctx); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}
	c.conn = tlsConn
	c.r = bufio.NewReader(tlsConn)
	c.w = bufio.NewWriter(tlsConn)
	return nil
}

// prefixConn replays bytes that were already consumed from the buffered
// reader before the TLS handshake.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Stat returns the message count and total size of the mailbox.
func (c *Client) Stat(ctx context.Context) (int, int64, error) {
	var count int
	var size int64
	err := c.run(ctx, func() error {
		text, _, err := c.exchange("STAT", "STAT", false)
		if err != nil {
			return err
		}
		count, size, err = parseStatResponse(text)
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return count, size, nil
}

// UIDL returns every message with its unique identifier and size.
func (c *Client) UIDL(ctx context.Context) ([]MessageInfo, error) {
	var out []MessageInfo
	err := c.run(ctx, func() error {
		_, lines, err := c.exchange("UIDL", "UIDL", true)
		if err != nil {
			return err
		}
		msgs, err := parseUIDLLines(lines)
		if err != nil {
			return err
		}
		// LIST is mandatory in POP3 and gives the sizes missing from UIDL.
		_, listLines, err := c.exchange("LIST", "LIST", true)
		if err != nil {
			return err
		}
		sizes, err := parseLISTLines(listLines)
		if err != nil {
			return err
		}
		for i := range msgs {
			msgs[i].Size = sizes[msgs[i].Number]
		}
		out = msgs
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Top returns the headers of a message plus lines body lines.
func (c *Client) Top(ctx context.Context, number, lines int) ([]byte, error) {
	if number <= 0 {
		return nil, errors.New("pop3client: top: message number must be greater than zero")
	}
	if lines < 0 {
		return nil, errors.New("pop3client: top: line count must not be negative")
	}
	var out []byte
	err := c.run(ctx, func() error {
		_, resp, err := c.exchange("TOP", fmt.Sprintf("TOP %d %d", number, lines), true)
		if err != nil {
			return err
		}
		out = encodeLines(resp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Retr returns the complete raw message.
func (c *Client) Retr(ctx context.Context, number int) ([]byte, error) {
	if number <= 0 {
		return nil, errors.New("pop3client: retr: message number must be greater than zero")
	}
	var out []byte
	err := c.run(ctx, func() error {
		_, resp, err := c.exchange("RETR", fmt.Sprintf("RETR %d", number), true)
		if err != nil {
			return err
		}
		out = encodeLines(resp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete marks a message for deletion. The deletion is committed by a
// successful Quit.
func (c *Client) Delete(ctx context.Context, number int) error {
	if number <= 0 {
		return errors.New("pop3client: delete: message number must be greater than zero")
	}
	return c.run(ctx, func() error {
		_, _, err := c.exchange("DELE", fmt.Sprintf("DELE %d", number), false)
		return err
	})
}

// Quit ends the session and commits pending deletions.
func (c *Client) Quit(ctx context.Context) error {
	err := c.run(ctx, func() error {
		_, _, err := c.exchange("QUIT", "QUIT", false)
		return err
	})
	c.closeConn()
	if err != nil {
		return err
	}
	return nil
}

// Close closes the connection without committing deletions.
func (c *Client) Close() error {
	c.closeConn()
	return c.closeErr
}

func (c *Client) closeConn() {
	c.closeOnce.Do(func() {
		c.closeErr = c.conn.Close()
		if errors.Is(c.closeErr, net.ErrClosed) {
			c.closeErr = nil
		}
	})
}

// run serializes a command, applies the operation deadline and interrupts
// blocked I/O when ctx is canceled.
func (c *Client) run(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	deadline := operationDeadline(ctx, c.timeout)
	_ = c.conn.SetDeadline(deadline)

	ctx, cancel := withTimeout(ctx, c.timeout)
	defer cancel()

	stop, watchDone := interruptOnDone(ctx, c.conn)
	err := fn()
	stop()
	<-watchDone
	_ = c.conn.SetDeadline(time.Time{})

	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		c.closeConn()
		return fmt.Errorf("pop3client: %w", ctxErr)
	}
	var srvErr *serverError
	if !errors.As(err, &srvErr) {
		// Transport or protocol-level failure: the session state is unknown.
		c.closeConn()
	}
	return fmt.Errorf("pop3client: %w", err)
}

// exchange sends line and reads the status response. When multiline is set it
// also reads the dot-terminated response body.
func (c *Client) exchange(label, line string, multiline bool) (string, []string, error) {
	if _, err := c.w.WriteString(line + "\r\n"); err != nil {
		return "", nil, err
	}
	if err := c.w.Flush(); err != nil {
		return "", nil, err
	}
	text, err := c.readStatusLine(label)
	if err != nil {
		return "", nil, err
	}
	if !multiline {
		return text, nil, nil
	}
	lines, err := c.readMultilineLines()
	if err != nil {
		return "", nil, err
	}
	return text, lines, nil
}

func (c *Client) readStatusLine(label string) (string, error) {
	line, err := c.readRawLine()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(line, "+OK") {
		return strings.TrimPrefix(strings.TrimPrefix(line, "+OK"), " "), nil
	}
	if strings.HasPrefix(line, "-ERR") {
		return "", &serverError{command: label, text: truncate(line, 200)}
	}
	return "", fmt.Errorf("unexpected response to %s: %q", label, truncate(line, 200))
}

func (c *Client) readRawLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}

// readMultilineLines reads a dot-terminated response, removing byte stuffing.
func (c *Client) readMultilineLines() ([]string, error) {
	var lines []string
	for {
		line, err := c.readRawLine()
		if err != nil {
			return lines, err
		}
		if line == "." {
			break
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func encodeLines(lines []string) []byte {
	if len(lines) == 0 {
		return []byte{}
	}
	var buf bytes.Buffer
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteString("\r\n")
	}
	return buf.Bytes()
}

func parseStatResponse(text string) (int, int64, error) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("malformed STAT response %q", truncate(text, 100))
	}
	count, err := strconv.Atoi(fields[0])
	if err != nil || count < 0 {
		return 0, 0, fmt.Errorf("malformed STAT count %q", fields[0])
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || size < 0 {
		return 0, 0, fmt.Errorf("malformed STAT size %q", fields[1])
	}
	return count, size, nil
}

// parseUIDLLines parses "number uid" rows.
func parseUIDLLines(lines []string) ([]MessageInfo, error) {
	out := make([]MessageInfo, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed UIDL row %q", truncate(line, 100))
		}
		number, err := strconv.Atoi(fields[0])
		if err != nil || number <= 0 {
			return nil, fmt.Errorf("malformed UIDL message number %q", fields[0])
		}
		out = append(out, MessageInfo{Number: number, UIDL: fields[1]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// parseLISTLines parses "number size" rows.
func parseLISTLines(lines []string) (map[int]int64, error) {
	out := make(map[int]int64, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed LIST row %q", truncate(line, 100))
		}
		number, err := strconv.Atoi(fields[0])
		if err != nil || number <= 0 {
			return nil, fmt.Errorf("malformed LIST message number %q", fields[0])
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("malformed LIST size %q", fields[1])
		}
		out[number] = size
	}
	return out, nil
}

type serverError struct {
	command string
	text    string
}

func (e *serverError) Error() string {
	return fmt.Sprintf("%s rejected: %s", e.command, e.text)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

func operationDeadline(ctx context.Context, timeout time.Duration) time.Time {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if ctxDeadline, ok := ctx.Deadline(); ok && (deadline.IsZero() || ctxDeadline.Before(deadline)) {
		deadline = ctxDeadline
	}
	return deadline
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

func validateConfig(cfg Config) error {
	if err := validateAddress(cfg.Address); err != nil {
		return err
	}
	if err := validateSecurity(cfg.Security); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Username) == "" {
		return errors.New("pop3client: username is required")
	}
	if strings.ContainsAny(cfg.Username, "\r\n") || strings.ContainsAny(cfg.Password, "\r\n") {
		return errors.New("pop3client: credentials must not contain line breaks")
	}
	return nil
}

// validateAddress checks a "host:port" or "[IPv6]:port" address.
func validateAddress(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("pop3client: address is required")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("pop3client: invalid address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("pop3client: invalid address %q: missing host", addr)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("pop3client: invalid address %q: port must be 1-65535", addr)
	}
	return nil
}

func validateSecurity(security string) error {
	switch security {
	case SecurityImplicitTLS, SecuritySTARTTLS, SecurityNone:
		return nil
	case "":
		return errors.New("pop3client: security must be implicit_tls, starttls or none")
	default:
		return fmt.Errorf("pop3client: unknown security %q", security)
	}
}
