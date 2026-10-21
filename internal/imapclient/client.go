// Package imapclient wraps the go-imap v2 client with the connection,
// authentication and mailbox operations MailQuay needs.
//
// Every method derives a per-operation deadline from Config.Timeout and the
// caller's context, and wraps network or protocol errors with %w. Fatal
// protocol errors close the underlying connection so that the next operation
// fails instead of using a desynchronized stream.
package imapclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	gimapc "github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"
)

const (
	// SecurityImplicitTLS connects with TLS from the first byte.
	SecurityImplicitTLS = "implicit_tls"
	// SecuritySTARTTLS upgrades a plaintext connection with STARTTLS.
	SecuritySTARTTLS = "starttls"
	// SecurityNone connects without transport encryption.
	SecurityNone = "none"

	defaultTimeout = 30 * time.Second
	maxUint32      = ^uint32(0)
)

// Address is an envelope address. Name may be empty.
type Address struct {
	Name  string
	Email string
}

// Config describes how to connect to an IMAP server.
type Config struct {
	// Address is "host:port" or "[IPv6]:port". The port is mandatory.
	Address string
	// Security is one of implicit_tls, starttls or none. TLS is never
	// inferred from the port.
	Security string
	// Username is the login name. It is required for LOGIN and XOAUTH2.
	Username string
	// Password is used for LOGIN when AccessToken is empty.
	Password string
	// AccessToken, when set, selects XOAUTH2 authentication.
	AccessToken string
	// Timeout bounds each operation. Zero means 30 seconds.
	Timeout time.Duration
}

// Folder is a mailbox returned by LIST.
type Folder struct {
	Path       string
	Name       string
	Delimiter  string
	Attributes []string
}

// Selected is the state returned by SELECT.
type Selected struct {
	UIDValidity   uint32
	UIDNext       uint32
	HighestModSeq uint64
}

// Envelope is the metadata MailQuay stores for a message.
type Envelope struct {
	UID    uint32
	Flags  []string
	Size   uint32
	ModSeq uint64

	MessageID  string
	InReplyTo  string
	References []string
	Subject    string

	From []Address
	To   []Address
	Cc   []Address
	Bcc  []Address

	SentAt       time.Time
	InternalDate time.Time

	// GMailID is reserved for the X-GM-MSGID extension. go-imap v2 does not
	// expose that FETCH item, so it is currently always empty.
	GMailID string
}

// FlagUpdate is a flag change for one message.
type FlagUpdate struct {
	UID    uint32
	Flags  []string
	ModSeq uint64
}

// MailboxStatus is the result of STATUS.
type MailboxStatus struct {
	Messages    uint32
	Unseen      uint32
	UIDNext     uint32
	UIDValidity uint32
}

// Client is an authenticated IMAP connection.
//
// It serializes operations: commands are never pipelined. Idle blocks other
// operations until it returns.
type Client struct {
	c       *gimapc.Client
	conn    net.Conn
	timeout time.Duration

	mu      sync.Mutex
	changes chan struct{}
	caps    imap.CapSet

	closeOnce sync.Once
	closeErr  error
}

// staticRSACipherSuites use the RSA key exchange. Go stopped offering them by
// default because they lack forward secrecy.
var staticRSACipherSuites = []uint16{
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
}

// ecdheCipherSuites mirrors Go's default TLS 1.2 preference order. Setting
// CipherSuites replaces the default list, so the ECDHE suites are spelled out
// to keep forward secrecy ahead of the static RSA fallback.
var ecdheCipherSuites = []uint16{
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
}

// imapTLSConfig builds the TLS configuration for host. The default suite list
// is left untouched for servers that accept it.
func imapTLSConfig(host string) *tls.Config {
	cfg := &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}
	if requiresStaticRSACiphers(host) {
		cfg.CipherSuites = append(append([]uint16{}, ecdheCipherSuites...), staticRSACipherSuites...)
	}
	return cfg
}

// requiresStaticRSACiphers reports whether host is a China Mobile mail IMAP
// endpoint. Their TLS gateway answers handshake_failure unless the
// ClientHello advertises a static RSA key exchange suite, even when ECDHE
// suites are offered.
func requiresStaticRSACiphers(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range []string{"139.com", "10086.cn"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// Dial connects, negotiates transport security, authenticates and returns a
// ready client.
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

	c := &Client{
		timeout: timeout,
		changes: make(chan struct{}, 1),
	}
	tlsConfig := imapTLSConfig(host)
	opts := &gimapc.Options{
		TLSConfig:   tlsConfig,
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
		UnilateralDataHandler: &gimapc.UnilateralDataHandler{
			Mailbox: func(*gimapc.UnilateralDataMailbox) { c.signal() },
			Expunge: func(uint32) { c.signal() },
			Fetch: func(msg *gimapc.FetchMessageData) {
				drainFetch(msg)
				c.signal()
			},
		},
	}
	dialer := &net.Dialer{Timeout: timeout}

	switch cfg.Security {
	case SecurityImplicitTLS:
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsConfig}
		conn, err := tlsDialer.DialContext(ctx, "tcp", cfg.Address)
		if err != nil {
			return nil, fmt.Errorf("imapclient: dial %s: %w", cfg.Address, err)
		}
		c.conn = conn
		c.c = gimapc.New(conn, opts)
	case SecuritySTARTTLS:
		conn, err := dialer.DialContext(ctx, "tcp", cfg.Address)
		if err != nil {
			return nil, fmt.Errorf("imapclient: dial %s: %w", cfg.Address, err)
		}
		c.conn = conn
		_ = conn.SetDeadline(operationDeadline(ctx, timeout))
		stop, done := interruptOnDone(ctx, conn)
		ic, err := gimapc.NewStartTLS(conn, opts)
		stop()
		<-done
		_ = conn.SetDeadline(time.Time{})
		if err != nil {
			conn.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("imapclient: starttls with %s: %w", cfg.Address, ctxErr)
			}
			return nil, fmt.Errorf("imapclient: starttls with %s: %w", cfg.Address, err)
		}
		c.c = ic
	default:
		conn, err := dialer.DialContext(ctx, "tcp", cfg.Address)
		if err != nil {
			return nil, fmt.Errorf("imapclient: dial %s: %w", cfg.Address, err)
		}
		c.conn = conn
		c.c = gimapc.New(conn, opts)
	}

	if err := c.waitGreeting(ctx); err != nil {
		c.disconnect()
		return nil, err
	}

	if err := c.authenticate(ctx, cfg); err != nil {
		c.disconnect()
		return nil, err
	}

	// Cache capabilities once, under a deadline. Servers may advertise a
	// different set after authentication. A nil set means the server did not
	// answer; extensions are then simply not used.
	_ = c.do(ctx, func() error {
		c.caps = c.c.Caps()
		return nil
	})
	select {
	case <-c.c.Closed():
		c.disconnect()
		return nil, errors.New("imapclient: connection closed during capability negotiation")
	default:
	}

	return c, nil
}

// Closed reports whether the underlying connection is gone. A fatal protocol
// error or an explicit Close makes it true; callers use it to discard pooled
// connections instead of reusing a desynchronized stream.
func (c *Client) Closed() bool {
	if c == nil || c.c == nil {
		return true
	}
	select {
	case <-c.c.Closed():
		return true
	default:
		return false
	}
}

// Close closes the connection. It is safe to call more than once.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.c != nil {
			c.closeErr = c.c.Close()
		} else if c.conn != nil {
			c.closeErr = c.conn.Close()
		}
		if errors.Is(c.closeErr, net.ErrClosed) || errors.Is(c.closeErr, io.ErrClosedPipe) {
			c.closeErr = nil
		}
	})
	return c.closeErr
}

func (c *Client) waitGreeting(ctx context.Context) error {
	return c.do(ctx, func() error {
		return c.c.WaitGreeting()
	})
}

func (c *Client) authenticate(ctx context.Context, cfg Config) error {
	return c.do(ctx, func() error {
		if cfg.AccessToken != "" {
			return c.c.Authenticate(&xoauth2Client{username: cfg.Username, token: cfg.AccessToken})
		}
		return c.c.Login(cfg.Username, cfg.Password).Wait()
	})
}

// List returns all mailboxes.
func (c *Client) List(ctx context.Context) ([]Folder, error) {
	specialUse := c.hasCap(imap.CapSpecialUse)
	folders, err := c.list(ctx, specialUse)
	if err != nil && specialUse && isStatusError(err) {
		// Some servers advertise SPECIAL-USE but reject the LIST return
		// option; retry with a plain LIST.
		folders, err = c.list(ctx, false)
	}
	if err != nil {
		return nil, err
	}
	return folders, nil
}

func (c *Client) list(ctx context.Context, specialUse bool) ([]Folder, error) {
	var folders []Folder
	err := c.do(ctx, func() error {
		cmd := c.c.List("", "*", &imap.ListOptions{ReturnSpecialUse: specialUse})
		items, err := cmd.Collect()
		if err != nil {
			return err
		}
		folders = make([]Folder, 0, len(items))
		for _, item := range items {
			if item == nil || item.Mailbox == "" {
				continue
			}
			delim := ""
			if item.Delim != 0 {
				delim = string(item.Delim)
			}
			attrs := make([]string, 0, len(item.Attrs))
			for _, attr := range item.Attrs {
				attrs = append(attrs, string(attr))
			}
			folders = append(folders, Folder{
				Path:       item.Mailbox,
				Name:       folderName(item.Mailbox, delim),
				Delimiter:  delim,
				Attributes: attrs,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return folders, nil
}

// Select opens a mailbox and returns its UID state.
func (c *Client) Select(ctx context.Context, path string) (Selected, error) {
	if path == "" {
		return Selected{}, errors.New("imapclient: select: empty mailbox path")
	}
	var out Selected
	err := c.do(ctx, func() error {
		opts := &imap.SelectOptions{CondStore: c.hasCap(imap.CapCondStore)}
		data, err := c.c.Select(path, opts).Wait()
		if err != nil {
			return err
		}
		out = Selected{
			UIDValidity:   data.UIDValidity,
			UIDNext:       uint32(data.UIDNext),
			HighestModSeq: data.HighestModSeq,
		}
		return nil
	})
	return out, err
}

// UIDSearchAll returns the UIDs of all messages in the selected mailbox.
func (c *Client) UIDSearchAll(ctx context.Context) ([]uint32, error) {
	return c.search(ctx, &imap.SearchCriteria{})
}

// UIDSearchSince returns the UIDs of messages whose internal date is on or
// after since.
func (c *Client) UIDSearchSince(ctx context.Context, since time.Time) ([]uint32, error) {
	if since.IsZero() {
		return c.UIDSearchAll(ctx)
	}
	return c.search(ctx, &imap.SearchCriteria{Since: since})
}

// UIDSearchNew returns the UIDs greater than or equal to fromUID. A zero
// fromUID searches every message.
//
// IMAP defines "n:*" to include at least the last message even when n is
// larger than every assigned UID, so callers should still filter the result
// against UIDs they already know.
func (c *Client) UIDSearchNew(ctx context.Context, fromUID uint32) ([]uint32, error) {
	if fromUID == 0 {
		return c.UIDSearchAll(ctx)
	}
	var set imap.UIDSet
	set.AddRange(imap.UID(fromUID), 0) // 0 encodes "*"
	return c.search(ctx, &imap.SearchCriteria{UID: []imap.UIDSet{set}})
}

func (c *Client) search(ctx context.Context, criteria *imap.SearchCriteria) ([]uint32, error) {
	var uids []uint32
	err := c.do(ctx, func() error {
		data, err := c.c.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return err
		}
		set, ok := data.All.(imap.UIDSet)
		if !ok {
			return nil
		}
		nums, ok := set.Nums()
		if !ok {
			// A dynamic set is a server bug; ignore it instead of panicking.
			return nil
		}
		uids = make([]uint32, 0, len(nums))
		for _, uid := range nums {
			uids = append(uids, uint32(uid))
		}
		return nil
	})
	return uids, err
}

// FetchEnvelopes returns metadata for uids, sorted by UID. Headers are fetched
// as raw bytes and parsed locally: the IMAP ENVELOPE item is a nested structure
// that some servers emit malformed, and a single bad envelope desynchronizes
// the whole fetch.
func (c *Client) FetchEnvelopes(ctx context.Context, uids []uint32) ([]Envelope, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if err := validateUIDs(uids); err != nil {
		return nil, err
	}
	var out []Envelope
	err := c.do(ctx, func() error {
		opts := &imap.FetchOptions{
			Flags:        true,
			RFC822Size:   true,
			InternalDate: true,
			ModSeq:       c.hasCap(imap.CapCondStore),
			BodySection: []*imap.FetchItemBodySection{{
				Specifier: imap.PartSpecifierHeader,
				Peek:      true,
			}},
		}
		bufs, err := c.c.Fetch(uidSet(uids), opts).Collect()
		if err != nil {
			return err
		}
		out = make([]Envelope, 0, len(bufs))
		for _, buf := range bufs {
			if buf == nil || buf.UID == 0 {
				continue
			}
			out = append(out, envelopeFromBuffer(buf))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func envelopeFromBuffer(buf *gimapc.FetchMessageBuffer) Envelope {
	env := Envelope{
		UID:          uint32(buf.UID),
		Flags:        flagStrings(buf.Flags),
		Size:         clampSize(buf.RFC822Size),
		ModSeq:       buf.ModSeq,
		InternalDate: buf.InternalDate,
	}
	for _, section := range buf.BodySection {
		if section.Section != nil && section.Section.Specifier == imap.PartSpecifierHeader {
			applyHeaders(&env, section.Bytes)
			break
		}
	}
	if env.SentAt.IsZero() {
		env.SentAt = buf.InternalDate
	}
	return env
}

// applyHeaders fills the envelope fields from a raw header block. Parsing
// locally keeps one malformed header from aborting the batch and never depends
// on the server's ENVELOPE encoding.
func applyHeaders(env *Envelope, raw []byte) {
	fields := headerFields(raw)
	env.MessageID = strings.TrimSpace(fields["message-id"])
	env.InReplyTo = firstMessageID(fields["in-reply-to"])
	env.Subject = decodeHeader(fields["subject"])
	env.From = parseAddressList(fields["from"])
	env.To = parseAddressList(fields["to"])
	env.Cc = parseAddressList(fields["cc"])
	env.Bcc = parseAddressList(fields["bcc"])
	env.References = parseReferences(raw)
	if at, ok := parseHeaderDate(fields["date"]); ok {
		env.SentAt = at
	}
}

// headerFields unfolds the header block and keeps the first occurrence of each
// field. Unparseable lines are skipped instead of failing the message.
func headerFields(raw []byte) map[string]string {
	fields := make(map[string]string, 16)
	current := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if current != "" {
				fields[current] += " " + strings.TrimSpace(line)
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			current = ""
			continue
		}
		current = strings.ToLower(strings.TrimSpace(name))
		if _, exists := fields[current]; !exists {
			fields[current] = strings.TrimSpace(value)
		}
	}
	return fields
}

func firstMessageID(value string) string {
	if start := strings.IndexByte(value, '<'); start >= 0 {
		if end := strings.IndexByte(value[start:], '>'); end > 0 {
			return value[start : start+end+1]
		}
	}
	return strings.TrimSpace(value)
}

func parseHeaderDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if at, err := mail.ParseDate(value); err == nil {
		return at, true
	}
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.ANSIC} {
		if at, err := time.Parse(layout, value); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

// parseAddressList decodes an address header. When the server or sender emits
// something the standard parser rejects, addresses are recovered one by one so
// the message is still stored.
func parseAddressList(value string) []Address {
	if value == "" {
		return nil
	}
	decoded := decodeHeader(value)
	if list, err := mail.ParseAddressList(decoded); err == nil {
		out := make([]Address, 0, len(list))
		for _, addr := range list {
			out = append(out, Address{Name: decodeHeader(addr.Name), Email: addr.Address})
		}
		return out
	}
	var out []Address
	for _, chunk := range strings.Split(decoded, ",") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		if addr, err := mail.ParseAddress(chunk); err == nil {
			out = append(out, Address{Name: decodeHeader(addr.Name), Email: addr.Address})
			continue
		}
		if email := bareAddress(chunk); email != "" {
			out = append(out, Address{Email: email})
		}
	}
	return out
}

func bareAddress(value string) string {
	if start := strings.IndexByte(value, '<'); start >= 0 {
		if end := strings.IndexByte(value[start:], '>'); end > 1 {
			return strings.TrimSpace(value[start+1 : start+end])
		}
	}
	if strings.Contains(value, "@") && !strings.ContainsAny(value, " \t;") {
		return strings.Trim(value, "\"'")
	}
	return ""
}

// FetchFlags returns the current flags for uids.
func (c *Client) FetchFlags(ctx context.Context, uids []uint32) ([]FlagUpdate, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if err := validateUIDs(uids); err != nil {
		return nil, err
	}
	var out []FlagUpdate
	err := c.do(ctx, func() error {
		opts := &imap.FetchOptions{
			Flags:  true,
			ModSeq: c.hasCap(imap.CapCondStore),
		}
		bufs, err := c.c.Fetch(uidSet(uids), opts).Collect()
		if err != nil {
			return err
		}
		out = make([]FlagUpdate, 0, len(bufs))
		for _, buf := range bufs {
			if buf == nil || buf.UID == 0 {
				continue
			}
			out = append(out, FlagUpdate{
				UID:    uint32(buf.UID),
				Flags:  flagStrings(buf.Flags),
				ModSeq: buf.ModSeq,
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FetchHeader returns the raw RFC 822 header for uid.
func (c *Client) FetchHeader(ctx context.Context, uid uint32) ([]byte, error) {
	return c.fetchSection(ctx, uid, imap.FetchItemBodySection{
		Specifier: imap.PartSpecifierHeader,
		Peek:      true,
	})
}

// FetchRaw returns the complete raw message for uid.
func (c *Client) FetchRaw(ctx context.Context, uid uint32) ([]byte, error) {
	return c.fetchSection(ctx, uid, imap.FetchItemBodySection{Peek: true})
}

func (c *Client) fetchSection(ctx context.Context, uid uint32, section imap.FetchItemBodySection) ([]byte, error) {
	if uid == 0 {
		return nil, errors.New("imapclient: fetch: uid must be greater than zero")
	}
	var out []byte
	found := false
	err := c.do(ctx, func() error {
		opts := &imap.FetchOptions{
			BodySection: []*imap.FetchItemBodySection{&section},
		}
		bufs, err := c.c.Fetch(imap.UIDSetNum(imap.UID(uid)), opts).Collect()
		if err != nil {
			return err
		}
		for _, buf := range bufs {
			if buf == nil || buf.UID != imap.UID(uid) {
				continue
			}
			for _, got := range buf.BodySection {
				if got.Section != nil && matchSection(&section, got.Section) {
					out = append([]byte(nil), got.Bytes...)
					found = true
					return nil
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("imapclient: message with uid %d not found", uid)
	}
	return out, nil
}

// StoreFlags adds or removes flags on uid.
func (c *Client) StoreFlags(ctx context.Context, uid uint32, flags []string, add bool) error {
	if uid == 0 {
		return errors.New("imapclient: store: uid must be greater than zero")
	}
	if len(flags) == 0 {
		return nil
	}
	return c.do(ctx, func() error {
		op := imap.StoreFlagsAdd
		if !add {
			op = imap.StoreFlagsDel
		}
		cmd := c.c.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{
			Op:     op,
			Silent: true,
			Flags:  flagList(flags),
		}, nil)
		return cmd.Close()
	})
}

// Move moves uid to dest. It uses MOVE when available and otherwise falls
// back to COPY + STORE \Deleted + EXPUNGE.
func (c *Client) Move(ctx context.Context, uid uint32, dest string) error {
	if uid == 0 {
		return errors.New("imapclient: move: uid must be greater than zero")
	}
	if dest == "" {
		return errors.New("imapclient: move: empty destination")
	}
	return c.do(ctx, func() error {
		_, err := c.c.Move(imap.UIDSetNum(imap.UID(uid)), dest).Wait()
		return err
	})
}

// Expunge permanently removes uids from the selected mailbox.
//
// UID EXPUNGE is used when the server supports UIDPLUS or IMAP4rev2. Otherwise
// the UIDs are marked \Deleted and a plain EXPUNGE runs; that fallback also
// expunges any other message already marked \Deleted in the mailbox.
func (c *Client) Expunge(ctx context.Context, uids []uint32) error {
	if c.c.Mailbox() == nil {
		return errors.New("imapclient: expunge: no mailbox selected")
	}
	if len(uids) == 0 {
		return c.do(ctx, func() error {
			return c.c.Expunge().Close()
		})
	}
	if err := validateUIDs(uids); err != nil {
		return err
	}
	return c.do(ctx, func() error {
		// Both UID EXPUNGE and EXPUNGE only remove messages flagged
		// \Deleted, so set the flag even when the caller already did.
		if err := c.storeDeleted(uids); err != nil {
			return err
		}
		if c.hasCap(imap.CapUIDPlus) || c.hasCap(imap.CapIMAP4rev2) {
			return c.c.UIDExpunge(uidSet(uids)).Close()
		}
		// Without UIDPLUS the fallback also expunges other \Deleted
		// messages in the mailbox.
		return c.c.Expunge().Close()
	})
}

func (c *Client) storeDeleted(uids []uint32) error {
	cmd := c.c.Store(uidSet(uids), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagDeleted},
	}, nil)
	return cmd.Close()
}

// Append adds raw to the mailbox and returns the new UID when the server
// reports it through UIDPLUS; otherwise it returns zero.
func (c *Client) Append(ctx context.Context, path string, flags []string, date time.Time, raw []byte) (uint32, error) {
	if path == "" {
		return 0, errors.New("imapclient: append: empty mailbox path")
	}
	var uid uint32
	err := c.do(ctx, func() error {
		opts := &imap.AppendOptions{Flags: flagList(flags)}
		if !date.IsZero() {
			opts.Time = date
		}
		cmd := c.c.Append(path, int64(len(raw)), opts)
		if len(raw) > 0 {
			if _, err := cmd.Write(raw); err != nil {
				_ = cmd.Close()
				return err
			}
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		data, err := cmd.Wait()
		if err != nil {
			return err
		}
		uid = uint32(data.UID)
		return nil
	})
	return uid, err
}

// EnsureMailbox creates path if it does not exist. Servers report an existing
// mailbox with a tagged error; that case is not an error here.
func (c *Client) EnsureMailbox(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("imapclient: create: empty mailbox path")
	}
	err := c.do(ctx, func() error {
		return c.c.Create(path, nil).Wait()
	})
	if err != nil && mailboxExistsError(err) {
		return nil
	}
	return err
}

// Status returns counters for path without selecting it.
func (c *Client) Status(ctx context.Context, path string) (MailboxStatus, error) {
	if path == "" {
		return MailboxStatus{}, errors.New("imapclient: status: empty mailbox path")
	}
	var out MailboxStatus
	err := c.do(ctx, func() error {
		data, err := c.c.Status(path, &imap.StatusOptions{
			NumMessages: true,
			NumUnseen:   true,
			UIDNext:     true,
			UIDValidity: true,
		}).Wait()
		if err != nil {
			return err
		}
		if data.NumMessages != nil {
			out.Messages = *data.NumMessages
		}
		if data.NumUnseen != nil {
			out.Unseen = *data.NumUnseen
		}
		out.UIDNext = uint32(data.UIDNext)
		out.UIDValidity = data.UIDValidity
		return nil
	})
	return out, err
}

// Idle runs one IDLE round on the selected mailbox.
//
// It returns true when the server advertised a change during the round. A
// timeout with no change is not an error; a canceled context is.
func (c *Client) Idle(ctx context.Context, timeout time.Duration) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	parent := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Drop a notification left over from an earlier command.
	select {
	case <-c.changes:
	default:
	}

	stop, watchDone := interruptOnDone(ctx, c.conn)
	idle, err := c.c.Idle()
	if err != nil {
		stop()
		<-watchDone
		_ = c.conn.SetDeadline(time.Time{})
		c.disconnect()
		return false, wrapContext(ctx, fmt.Errorf("idle: %w", err))
	}

	changed := false
	select {
	case <-c.changes:
		changed = true
	case <-ctx.Done():
	}

	// Stop the watcher first: on a normal timeout its deadline interrupt
	// would otherwise prevent DONE from being written.
	stop()
	<-watchDone

	if parent.Err() != nil {
		c.disconnect()
		_ = idle.Close()
		return false, fmt.Errorf("imapclient: idle: %w", parent.Err())
	}

	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	closeErr := idle.Close()
	waitErr := idle.Wait()
	_ = c.conn.SetDeadline(time.Time{})

	if err := errors.Join(closeErr, waitErr); err != nil {
		c.disconnect()
		return changed, fmt.Errorf("imapclient: idle: %w", err)
	}
	return changed, nil
}

func (c *Client) do(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

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
		c.disconnect()
		return fmt.Errorf("imapclient: %w", ctxErr)
	}
	if isFatal(err) {
		c.disconnect()
	}
	return fmt.Errorf("imapclient: %w", err)
}

func (c *Client) signal() {
	select {
	case c.changes <- struct{}{}:
	default:
	}
}

func (c *Client) disconnect() {
	if c.c != nil {
		_ = c.c.Close()
	} else if c.conn != nil {
		_ = c.conn.Close()
	}
}

func (c *Client) hasCap(cap imap.Cap) bool {
	return c.caps.Has(cap)
}

// withTimeout derives a bounded context. A non-positive timeout keeps the
// caller's deadline only.
func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithCancel(ctx)
}

// operationDeadline returns the earliest of the caller deadline and the
// per-operation timeout.
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

// interruptOnDone makes blocked network reads return when ctx is canceled.
// The returned stop function must be called before the caller resets the
// connection deadline.
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

func wrapContext(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("imapclient: %w", ctxErr)
	}
	return fmt.Errorf("imapclient: %w", err)
}

func isFatal(err error) bool {
	var imapErr *imap.Error
	return !errors.As(err, &imapErr)
}

func isStatusError(err error) bool {
	var imapErr *imap.Error
	return errors.As(err, &imapErr)
}

func drainFetch(msg *gimapc.FetchMessageData) {
	if msg == nil {
		return
	}
	for {
		item := msg.Next()
		if item == nil {
			return
		}
		switch data := item.(type) {
		case gimapc.FetchItemDataBodySection:
			if data.Literal != nil {
				_, _ = io.Copy(io.Discard, data.Literal)
			}
		case gimapc.FetchItemDataBinarySection:
			if data.Literal != nil {
				_, _ = io.Copy(io.Discard, data.Literal)
			}
		}
	}
}

func matchSection(want, got *imap.FetchItemBodySection) bool {
	if want.Specifier != got.Specifier {
		return false
	}
	if len(want.Part) != len(got.Part) {
		return false
	}
	for i := range want.Part {
		if want.Part[i] != got.Part[i] {
			return false
		}
	}
	return true
}

func uidSet(uids []uint32) imap.UIDSet {
	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(imap.UID(uid))
	}
	return set
}

func flagList(flags []string) []imap.Flag {
	if len(flags) == 0 {
		return nil
	}
	out := make([]imap.Flag, 0, len(flags))
	for _, flag := range flags {
		if flag == "" {
			continue
		}
		out = append(out, imap.Flag(flag))
	}
	return out
}

func flagStrings(flags []imap.Flag) []string {
	if len(flags) == 0 {
		return nil
	}
	out := make([]string, 0, len(flags))
	for _, flag := range flags {
		out = append(out, string(flag))
	}
	return out
}

func decodeHeader(s string) string {
	if s == "" || !strings.Contains(s, "=?") {
		return s
	}
	decoder := &mime.WordDecoder{CharsetReader: charset.Reader}
	decoded, err := decoder.DecodeHeader(s)
	if err != nil {
		return s
	}
	return decoded
}

// parseReferences extracts message IDs from a References header. It tolerates
// folded headers and missing angle brackets.
func parseReferences(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var value strings.Builder
	inHeader := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if inHeader {
				value.WriteByte(' ')
				value.WriteString(strings.TrimSpace(line))
			}
			continue
		}
		inHeader = false
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "References") {
			inHeader = true
			value.WriteString(strings.TrimSpace(rest))
		}
	}
	text := value.String()
	if text == "" {
		return nil
	}

	var refs []string
	for {
		start := strings.IndexByte(text, '<')
		if start < 0 {
			break
		}
		end := strings.IndexByte(text[start:], '>')
		if end < 0 {
			break
		}
		id := strings.TrimSpace(text[start+1 : start+end])
		if id != "" {
			refs = append(refs, id)
		}
		text = text[start+end+1:]
	}
	if len(refs) == 0 {
		for _, field := range strings.FieldsFunc(text, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		}) {
			field = strings.Trim(field, "<>")
			if field != "" {
				refs = append(refs, field)
			}
		}
	}
	return refs
}

func folderName(path, delimiter string) string {
	if delimiter == "" {
		return path
	}
	if idx := strings.LastIndex(path, delimiter); idx >= 0 && idx+len(delimiter) < len(path) {
		return path[idx+len(delimiter):]
	}
	return path
}

func clampSize(size int64) uint32 {
	if size <= 0 {
		return 0
	}
	if size > int64(maxUint32) {
		return maxUint32
	}
	return uint32(size)
}

func mailboxExistsError(err error) bool {
	if err == nil {
		return false
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		return false
	}
	if imapErr.Code == imap.ResponseCodeAlreadyExists {
		return true
	}
	text := strings.ToLower(imapErr.Text)
	return strings.Contains(text, "already exists") || strings.Contains(text, "already exist") ||
		strings.Contains(text, "exists")
}

func validateUIDs(uids []uint32) error {
	for _, uid := range uids {
		if uid == 0 {
			return errors.New("imapclient: uid must be greater than zero")
		}
	}
	return nil
}

func validateConfig(cfg Config) error {
	if err := validateAddress(cfg.Address); err != nil {
		return err
	}
	if err := validateSecurity(cfg.Security); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Username) == "" {
		return errors.New("imapclient: username is required")
	}
	if cfg.AccessToken == "" && cfg.Password == "" {
		return errors.New("imapclient: password or access token is required")
	}
	return nil
}

// validateAddress checks a "host:port" or "[IPv6]:port" address.
func validateAddress(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("imapclient: address is required")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("imapclient: invalid address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("imapclient: invalid address %q: missing host", addr)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("imapclient: invalid address %q: port must be 1-65535", addr)
	}
	return nil
}

func validateSecurity(security string) error {
	switch security {
	case SecurityImplicitTLS, SecuritySTARTTLS, SecurityNone:
		return nil
	case "":
		return errors.New("imapclient: security must be implicit_tls, starttls or none")
	default:
		return fmt.Errorf("imapclient: unknown security %q", security)
	}
}
