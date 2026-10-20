package compose

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

type attachmentBlob struct {
	Filename    string
	ContentType string
	Data        []byte
}

type messageParts struct {
	From         mail.Address
	To           []mail.Address
	Cc           []mail.Address
	Bcc          []mail.Address
	ReplyTo      string
	Organization string
	Subject      string
	MessageID    string
	InReplyTo    string
	References   []string
	Date         time.Time
	Text         string
	HTML         string
	Attachments  []attachmentBlob
}

// buildMIME assembles an RFC 5322 message with an alternative body and attachments.
func buildMIME(parts messageParts) ([]byte, error) {
	var buf bytes.Buffer
	writeHeader := func(key, value string) {
		if value == "" {
			return
		}
		buf.WriteString(key)
		buf.WriteString(": ")
		buf.WriteString(value)
		buf.WriteString("\r\n")
	}
	writeHeader("From", parts.From.String())
	if len(parts.To) > 0 {
		writeHeader("To", joinAddresses(parts.To))
	}
	if len(parts.Cc) > 0 {
		writeHeader("Cc", joinAddresses(parts.Cc))
	}
	if len(parts.Bcc) > 0 {
		writeHeader("Bcc", joinAddresses(parts.Bcc))
	}
	if parts.ReplyTo != "" {
		writeHeader("Reply-To", parts.ReplyTo)
	}
	if parts.Organization != "" {
		writeHeader("Organization", mime.QEncoding.Encode("utf-8", parts.Organization))
	}
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", parts.Subject))
	writeHeader("Date", parts.Date.Format(time.RFC1123Z))
	if parts.MessageID != "" {
		writeHeader("Message-ID", parts.MessageID)
	}
	if parts.InReplyTo != "" {
		writeHeader("In-Reply-To", ensureAngle(parts.InReplyTo))
	}
	if len(parts.References) > 0 {
		refs := make([]string, 0, len(parts.References))
		for _, ref := range parts.References {
			refs = append(refs, ensureAngle(ref))
		}
		writeHeader("References", strings.Join(refs, " "))
	}
	writeHeader("MIME-Version", "1.0")

	body, bodyType, err := buildBody(parts)
	if err != nil {
		return nil, err
	}

	if len(parts.Attachments) > 0 {
		var outer bytes.Buffer
		writer := multipart.NewWriter(&outer)
		writeHeader("Content-Type", "multipart/mixed; boundary=\""+writer.Boundary()+"\"")
		buf.WriteString("\r\n")
		headerPart, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {bodyType},
			"Content-Transfer-Encoding": {"8bit"},
		})
		if err != nil {
			return nil, err
		}
		if _, err := headerPart.Write(body); err != nil {
			return nil, err
		}
		for _, att := range parts.Attachments {
			ctype := att.ContentType
			if ctype == "" {
				ctype = "application/octet-stream"
			}
			filename := att.Filename
			if filename == "" {
				filename = "attachment"
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Type", mime.FormatMediaType(ctype, map[string]string{"name": filename}))
			h.Set("Content-Transfer-Encoding", "base64")
			h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
			part, err := writer.CreatePart(h)
			if err != nil {
				return nil, err
			}
			encoded := base64.StdEncoding.EncodeToString(att.Data)
			for len(encoded) > 76 {
				if _, err := part.Write([]byte(encoded[:76] + "\r\n")); err != nil {
					return nil, err
				}
				encoded = encoded[76:]
			}
			if _, err := part.Write([]byte(encoded + "\r\n")); err != nil {
				return nil, err
			}
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		buf.Write(outer.Bytes())
		return buf.Bytes(), nil
	}
	writeHeader("Content-Type", bodyType)
	buf.WriteString("\r\n")
	buf.Write(body)
	return buf.Bytes(), nil
}

func buildBody(parts messageParts) ([]byte, string, error) {
	hasText := strings.TrimSpace(parts.Text) != ""
	hasHTML := strings.TrimSpace(parts.HTML) != ""
	if !hasText && !hasHTML {
		return []byte("\r\n"), "text/plain; charset=utf-8", nil
	}
	if hasText && !hasHTML {
		return []byte(toCRLF(parts.Text) + "\r\n"), "text/plain; charset=utf-8", nil
	}
	if !hasText && hasHTML {
		return []byte(toCRLF(parts.HTML) + "\r\n"), "text/html; charset=utf-8", nil
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", "text/plain; charset=utf-8")
	textHeader.Set("Content-Transfer-Encoding", "8bit")
	textPart, err := writer.CreatePart(textHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := textPart.Write([]byte(toCRLF(parts.Text) + "\r\n")); err != nil {
		return nil, "", err
	}
	htmlHeader := textproto.MIMEHeader{}
	htmlHeader.Set("Content-Type", "text/html; charset=utf-8")
	htmlHeader.Set("Content-Transfer-Encoding", "8bit")
	htmlPart, err := writer.CreatePart(htmlHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := htmlPart.Write([]byte(toCRLF(parts.HTML) + "\r\n")); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), "multipart/alternative; boundary=\"" + writer.Boundary() + "\"", nil
}

func joinAddresses(addrs []mail.Address) string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.String())
	}
	return strings.Join(out, ", ")
}

func ensureAngle(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if !strings.HasPrefix(id, "<") {
		id = "<" + id
	}
	if !strings.HasSuffix(id, ">") {
		id += ">"
	}
	return id
}

func toCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func parseAddressList(values []string) ([]mail.Address, error) {
	out := make([]mail.Address, 0, len(values))
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		addr, err := mail.ParseAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q: %w", raw, err)
		}
		out = append(out, *addr)
	}
	return out, nil
}
