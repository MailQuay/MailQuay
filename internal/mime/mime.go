// Package mime parses raw RFC 5322 messages into metadata, body text and
// attachment descriptors. Parsing is defensive: a malformed header, address or
// body part never aborts the whole message.
package mime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	stdmime "mime"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers charset decoders
	"github.com/emersion/go-message/mail"
)

// Address is a mail address with an optional display name.
type Address struct {
	Name  string
	Email string
}

// Attachment describes one leaf body part that is not a readable text body.
type Attachment struct {
	PartPath    string // IMAP-style part path, e.g. "1", "2.1"
	Filename    string
	ContentType string
	Disposition string
	ContentID   string
	Size        int64
	Data        []byte // only populated when Options.KeepAttachmentData
}

// Parsed is the result of parsing a raw message.
type Parsed struct {
	MessageID      string
	InReplyTo      string
	ReplyTo        string
	References     []string
	Subject        string
	From           Address
	To, Cc, Bcc    []Address
	SentAt         time.Time // zero when missing or unparseable
	Text           string
	HTML           string // sanitized HTML
	HasAttachments bool
	Attachments    []Attachment
}

// Options controls body extraction.
type Options struct {
	// MaxAttachmentBytes bounds how much attachment data is retained. Bodies
	// larger than the limit are still measured and described, but Data is left
	// nil. Zero or negative means unlimited.
	MaxAttachmentBytes int64
	// KeepAttachmentData controls whether attachment Data is populated.
	KeepAttachmentData bool
	// KeepInlineImages keeps decoded data for inline images referenced by the
	// message body, even when other attachment data is discarded.
	KeepInlineImages bool
}

// Parse parses a raw message. Only a completely unreadable header produces an
// error; malformed individual headers or parts are tolerated.
func Parse(raw []byte, opts Options) (*Parsed, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("mime: empty message")
	}
	entity, err := message.Read(bytes.NewReader(raw))
	if entity == nil {
		if err == nil {
			err = errors.New("empty message")
		}
		return nil, fmt.Errorf("mime: read message: %w", err)
	}

	parsed := &Parsed{}
	readHeaders(entity.Header, parsed)

	c := collector{opts: opts, parsed: parsed}
	c.walk(entity, "")

	if parsed.Text == "" && parsed.HTML != "" {
		parsed.Text = textFromHTML(parsed.HTML)
	}
	parsed.HasAttachments = len(parsed.Attachments) > 0
	return parsed, nil
}

func readHeaders(header message.Header, parsed *Parsed) {
	h := mail.Header{Header: header}

	if id, err := h.MessageID(); err == nil && id != "" {
		parsed.MessageID = id
	} else if id := messageID(header.Get("Message-Id")); id != "" {
		parsed.MessageID = id
	}

	if list, err := h.MsgIDList("In-Reply-To"); err == nil && len(list) > 0 {
		parsed.InReplyTo = list[0]
	} else if list := msgIDList(header.Get("In-Reply-To")); len(list) > 0 {
		parsed.InReplyTo = list[0]
	}

	if list, err := h.MsgIDList("References"); err == nil && len(list) > 0 {
		parsed.References = list
	} else {
		parsed.References = msgIDList(header.Get("References"))
	}

	if replyTo := strings.TrimSpace(header.Get("Reply-To")); replyTo != "" {
		parsed.ReplyTo = replyTo
	}

	if subject, err := h.Subject(); err == nil {
		parsed.Subject = subject
	} else {
		parsed.Subject = decodeHeaderWord(header.Get("Subject"))
	}

	if from := parseAddresses(header.Get("From")); len(from) > 0 {
		parsed.From = from[0]
	}
	parsed.To = parseAddresses(header.Get("To"))
	parsed.Cc = parseAddresses(header.Get("Cc"))
	parsed.Bcc = parseAddresses(header.Get("Bcc"))

	if sentAt, err := h.Date(); err == nil {
		parsed.SentAt = sentAt
	}
}

func parseAddresses(value string) []Address {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if list, err := mail.ParseAddressList(value); err == nil {
		return convertAddresses(list)
	}

	// Fall back to per-entry parsing so one bad address doesn't discard the
	// rest of the header.
	parts := strings.Split(value, ",")
	out := make([]Address, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if addr, err := mail.ParseAddress(part); err == nil {
			out = append(out, Address{Name: addr.Name, Email: addr.Address})
			continue
		}
		out = append(out, Address{Name: "", Email: messageID(part)})
	}
	return out
}

func convertAddresses(list []*mail.Address) []Address {
	out := make([]Address, 0, len(list))
	for _, addr := range list {
		if addr == nil {
			continue
		}
		out = append(out, Address{Name: addr.Name, Email: addr.Address})
	}
	return out
}

// messageID strips angle brackets and surrounding whitespace from a single
// identifier.
func messageID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if start := strings.IndexByte(value, '<'); start >= 0 {
		if end := strings.IndexByte(value[start:], '>'); end > 0 {
			return value[start+1 : start+end]
		}
		return strings.Trim(value[start+1:], "<> ")
	}
	return strings.Trim(value, "<> ")
}

func msgIDList(value string) []string {
	var out []string
	rest := value
	for {
		start := strings.IndexByte(rest, '<')
		if start < 0 {
			return out
		}
		rest = rest[start:]
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return out
		}
		id := strings.TrimSpace(rest[1:end])
		if id != "" {
			out = append(out, id)
		}
		rest = rest[end+1:]
	}
}

func decodeHeaderWord(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	decoder := &stdmime.WordDecoder{CharsetReader: message.CharsetReader}
	if decoded, err := decoder.DecodeHeader(value); err == nil {
		return decoded
	}
	return value
}

type collector struct {
	opts    Options
	parsed  *Parsed
	textSet bool
	htmlSet bool
}

func (c *collector) walk(entity *message.Entity, path string) {
	if entity == nil {
		return
	}
	reader := entity.MultipartReader()
	if reader == nil {
		c.leaf(entity, path)
		return
	}
	defer reader.Close()

	for number := 1; ; number++ {
		part, err := reader.NextPart()
		if part == nil {
			return
		}
		childPath := strconv.Itoa(number)
		if path != "" {
			childPath = path + "." + childPath
		}
		c.walk(part, childPath)
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return
		}
	}
}

func (c *collector) leaf(entity *message.Entity, path string) {
	if path == "" {
		path = "1"
	}
	mediaType, params := entityContentType(entity)
	disposition, dispParams := entityDisposition(entity)
	filename := decodeHeaderWord(firstNonEmpty(dispParams["filename"], params["name"]))
	contentID := strings.Trim(strings.TrimSpace(entity.Header.Get("Content-Id")), "<>")

	if isAttachment(mediaType, filename, disposition) {
		inlineImage := strings.HasPrefix(mediaType, "image/") &&
			(disposition == "inline" || contentID != "")
		data, size := c.readAttachment(
			entity.Body,
			c.opts.KeepAttachmentData || (c.opts.KeepInlineImages && inlineImage),
		)
		c.parsed.Attachments = append(c.parsed.Attachments, Attachment{
			PartPath:    path,
			Filename:    filename,
			ContentType: mediaType,
			Disposition: disposition,
			ContentID:   contentID,
			Size:        size,
			Data:        data,
		})
		return
	}

	body, _ := io.ReadAll(entity.Body)
	if len(body) == 0 {
		return
	}
	switch mediaType {
	case "text/plain":
		if !c.textSet {
			c.parsed.Text = string(body)
			c.textSet = true
		}
	case "text/html":
		if !c.htmlSet {
			c.parsed.HTML = SanitizeHTML(string(body))
			c.htmlSet = true
		}
	}
}

func (c *collector) readAttachment(body io.Reader, keep bool) ([]byte, int64) {
	limit := c.opts.MaxAttachmentBytes
	if !keep {
		size, _ := io.Copy(io.Discard, body)
		return nil, size
	}
	if limit <= 0 {
		data, _ := io.ReadAll(body)
		return data, int64(len(data))
	}

	data, _ := io.ReadAll(io.LimitReader(body, limit+1))
	if int64(len(data)) > limit {
		rest, _ := io.Copy(io.Discard, body)
		return nil, int64(len(data)) + rest
	}
	return data, int64(len(data))
}

func isAttachment(mediaType, filename, disposition string) bool {
	if disposition == "attachment" || filename != "" {
		return true
	}
	if strings.HasPrefix(mediaType, "image/") {
		return true
	}
	return mediaType != "text/plain" && mediaType != "text/html"
}

func entityContentType(entity *message.Entity) (string, map[string]string) {
	raw := entity.Header.Get("Content-Type")
	mediaType, params, err := entity.Header.ContentType()
	if err != nil || strings.Contains(mediaType, ";") {
		base := strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
		if base == "" {
			base = "text/plain"
		}
		return strings.ToLower(base), headerParams(raw)
	}
	if mediaType == "" {
		mediaType = "text/plain"
	}
	return strings.ToLower(mediaType), params
}

func entityDisposition(entity *message.Entity) (string, map[string]string) {
	raw := entity.Header.Get("Content-Disposition")
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	disposition, params, err := entity.Header.ContentDisposition()
	if err != nil || strings.Contains(disposition, ";") || strings.ContainsAny(disposition, " \t") {
		base := strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
		return strings.ToLower(base), headerParams(raw)
	}
	return strings.ToLower(strings.TrimSpace(disposition)), params
}

func headerParams(raw string) map[string]string {
	_, params, err := stdmime.ParseMediaType(raw)
	if err != nil {
		return nil
	}
	return params
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
