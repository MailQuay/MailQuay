package compose

import (
	"bytes"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestBuildMIMEWithAttachment(t *testing.T) {
	raw, err := buildMIME(messageParts{
		From:    mail.Address{Name: "Sender", Address: "sender@example.com"},
		To:      []mail.Address{{Address: "to@example.com"}},
		Subject: "Hello 世界", MessageID: "<id@example.com>", Date: time.Unix(1700000000, 0),
		Text: "plain", HTML: "<p>html</p>",
		Attachments: []attachmentBlob{{Filename: "a.txt", ContentType: "text/plain", Data: []byte("data")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("Message-ID"); got != "<id@example.com>" {
		t.Errorf("Message-ID = %q", got)
	}
	if !strings.HasPrefix(msg.Header.Get("Content-Type"), "multipart/mixed") {
		t.Errorf("Content-Type = %q", msg.Header.Get("Content-Type"))
	}
	if msg.Header.Get("Subject") == "" {
		t.Error("missing subject")
	}
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(msg.Body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), "a.txt") || !strings.Contains(body.String(), "html") {
		t.Error("multipart body missing parts")
	}
}

func TestBuildMIMETextOnly(t *testing.T) {
	raw, err := buildMIME(messageParts{
		From: mail.Address{Address: "a@example.com"}, Subject: "s", Text: "line1\nline2",
		Date: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("line1\r\nline2")) {
		t.Error("body was not normalized to CRLF")
	}
	if bytes.Contains(raw, []byte("Bcc:")) {
		t.Error("Bcc leaked when empty")
	}
}
