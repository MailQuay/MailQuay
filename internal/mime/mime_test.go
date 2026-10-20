package mime_test

import (
	"strings"
	"testing"
	"time"

	mailmime "github.com/mailquay/mailquay/internal/mime"
)

const multipartMessage = "From: =?UTF-8?Q?Jos=C3=A9?= <jose@example.com>\r\n" +
	"To: Alice <alice@example.com>, \"Bob\" <bob@example.com>\r\n" +
	"Cc: carol@example.com\r\n" +
	"Subject: =?UTF-8?Q?Hello_=E4=BD=A0=E5=A5=BD?=\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
	"Message-ID: <abc@example.com>\r\n" +
	"In-Reply-To: <parent@example.com>\r\n" +
	"References: <root@example.com> <parent@example.com>\r\n" +
	"Content-Type: multipart/mixed; boundary=\"mix\"\r\n" +
	"\r\n" +
	"--mix\r\n" +
	"Content-Type: multipart/alternative; boundary=\"alt\"\r\n" +
	"\r\n" +
	"--alt\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"Hello world\r\n" +
	"--alt\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: 8bit\r\n" +
	"\r\n" +
	"<html><body><p>Hello <b>world</b></p><script>alert(1)</script>" +
	"<img src=\"https://tracker.example/p.gif\"></body></html>\r\n" +
	"--alt--\r\n" +
	"--mix\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"=?UTF-8?Q?report_=E2=9C=93.pdf?=\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"Content-ID: <att-1@example.com>\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--mix\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-Disposition: inline\r\n" +
	"Content-ID: <img-1@example.com>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--mix--\r\n"

func TestParseMultipart(t *testing.T) {
	parsed, err := mailmime.Parse([]byte(multipartMessage), mailmime.Options{KeepAttachmentData: true})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if parsed.MessageID != "abc@example.com" {
		t.Errorf("MessageID = %q", parsed.MessageID)
	}
	if parsed.InReplyTo != "parent@example.com" {
		t.Errorf("InReplyTo = %q", parsed.InReplyTo)
	}
	if got := strings.Join(parsed.References, ","); got != "root@example.com,parent@example.com" {
		t.Errorf("References = %q", got)
	}
	if parsed.Subject != "Hello 你好" {
		t.Errorf("Subject = %q", parsed.Subject)
	}
	if parsed.From.Name != "José" || parsed.From.Email != "jose@example.com" {
		t.Errorf("From = %+v", parsed.From)
	}
	if len(parsed.To) != 2 || parsed.To[0].Email != "alice@example.com" || parsed.To[1].Name != "Bob" {
		t.Errorf("To = %+v", parsed.To)
	}
	if len(parsed.Cc) != 1 || parsed.Cc[0].Email != "carol@example.com" {
		t.Errorf("Cc = %+v", parsed.Cc)
	}
	wantTime := time.Date(2006, 1, 2, 15, 4, 5, 0, time.FixedZone("", -7*60*60))
	if !parsed.SentAt.Equal(wantTime) {
		t.Errorf("SentAt = %v, want %v", parsed.SentAt, wantTime)
	}
	if !strings.Contains(parsed.Text, "Hello world") {
		t.Errorf("Text = %q", parsed.Text)
	}
	if strings.Contains(parsed.HTML, "<script") || !strings.Contains(parsed.HTML, "<b>world</b>") {
		t.Errorf("HTML = %q", parsed.HTML)
	}
	if !strings.Contains(parsed.HTML, `data-src="https://tracker.example/p.gif"`) {
		t.Errorf("HTML image URL not rewritten: %q", parsed.HTML)
	}
	if strings.Contains(parsed.HTML, ` src=`) {
		t.Errorf("HTML still has a loadable src: %q", parsed.HTML)
	}

	if !parsed.HasAttachments || len(parsed.Attachments) != 2 {
		t.Fatalf("Attachments = %+v", parsed.Attachments)
	}

	pdf := parsed.Attachments[0]
	if pdf.PartPath != "2" {
		t.Errorf("pdf PartPath = %q", pdf.PartPath)
	}
	if pdf.Filename != "report ✓.pdf" {
		t.Errorf("pdf Filename = %q", pdf.Filename)
	}
	if pdf.ContentType != "application/pdf" || pdf.Disposition != "attachment" {
		t.Errorf("pdf metadata = %+v", pdf)
	}
	if pdf.ContentID != "att-1@example.com" {
		t.Errorf("pdf ContentID = %q", pdf.ContentID)
	}
	if pdf.Size != 9 || string(pdf.Data) != "%PDF-1.4\n" {
		t.Errorf("pdf size/data = %d/%q", pdf.Size, pdf.Data)
	}

	image := parsed.Attachments[1]
	if image.PartPath != "3" || image.ContentType != "image/png" ||
		image.Disposition != "inline" || image.ContentID != "img-1@example.com" {
		t.Errorf("image metadata = %+v", image)
	}
	if image.Size != 8 || len(image.Data) != 8 {
		t.Errorf("image size/data = %d/%d", image.Size, len(image.Data))
	}
}

func TestParseAttachmentDataOptions(t *testing.T) {
	parsed, err := mailmime.Parse([]byte(multipartMessage), mailmime.Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, att := range parsed.Attachments {
		if att.Data != nil {
			t.Errorf("%s: Data should be omitted, got %d bytes", att.PartPath, len(att.Data))
		}
		if att.Size == 0 {
			t.Errorf("%s: Size should still be measured", att.PartPath)
		}
	}

	parsed, err = mailmime.Parse([]byte(multipartMessage), mailmime.Options{
		KeepAttachmentData: true,
		MaxAttachmentBytes: 5,
	})
	if err != nil {
		t.Fatalf("Parse with limit: %v", err)
	}
	for _, att := range parsed.Attachments {
		if att.Data != nil {
			t.Errorf("%s: Data should be dropped above the limit", att.PartPath)
		}
	}
	if parsed.Attachments[0].Size != 9 || parsed.Attachments[1].Size != 8 {
		t.Errorf("sizes = %d/%d", parsed.Attachments[0].Size, parsed.Attachments[1].Size)
	}
}

func TestParseSinglePartAttachmentPath(t *testing.T) {
	raw := "Subject: file\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n" +
		"\r\n" +
		"payload"
	parsed, err := mailmime.Parse([]byte(raw), mailmime.Options{KeepAttachmentData: true})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("Attachments = %+v", parsed.Attachments)
	}
	att := parsed.Attachments[0]
	if att.PartPath != "1" || att.Filename != "a.bin" || string(att.Data) != "payload" {
		t.Errorf("attachment = %+v", att)
	}
	if parsed.Text != "" || parsed.HTML != "" {
		t.Errorf("unexpected body text = %q / %q", parsed.Text, parsed.HTML)
	}
}

func TestParseNestedPartPath(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=outer\r\n" +
		"\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/related; boundary=inner\r\n" +
		"\r\n" +
		"--inner\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>x</p>\r\n" +
		"--inner\r\n" +
		"Content-Type: image/png\r\n" +
		"Content-Disposition: inline\r\n" +
		"Content-ID: <img@example.com>\r\n" +
		"\r\n" +
		"pngdata\r\n" +
		"--inner--\r\n" +
		"--outer--\r\n"
	parsed, err := mailmime.Parse([]byte(raw), mailmime.Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Attachments) != 1 || parsed.Attachments[0].PartPath != "1.2" {
		t.Fatalf("Attachments = %+v", parsed.Attachments)
	}
	if parsed.HTML == "" {
		t.Error("expected HTML body")
	}
}

func TestParseMalformedHeaders(t *testing.T) {
	raw := "From: not an address, Bad <bad@example.com\r\n" +
		"Date: not-a-date\r\n" +
		"Subject: =?x-unknown?Q?abc?=\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"body"
	parsed, err := mailmime.Parse([]byte(raw), mailmime.Options{})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(parsed.From.Email) == 0 {
		t.Errorf("From = %+v", parsed.From)
	}
	if !parsed.SentAt.IsZero() {
		t.Errorf("SentAt = %v, want zero", parsed.SentAt)
	}
	if parsed.Text != "body" {
		t.Errorf("Text = %q", parsed.Text)
	}
}

func TestParseHTMLOnlyFallbackText(t *testing.T) {
	raw := "Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><head><title>T</title><style>p{color:red}</style></head>" +
		"<body><p>Hello <b>World</b></p><script>evil()</script><br>again</body></html>"
	parsed, err := mailmime.Parse([]byte(raw), mailmime.Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if strings.Contains(parsed.Text, "evil") {
		t.Errorf("Text contains script: %q", parsed.Text)
	}
	if !strings.Contains(parsed.Text, "Hello World") || !strings.Contains(parsed.Text, "again") {
		t.Errorf("Text = %q", parsed.Text)
	}
	if !strings.Contains(parsed.HTML, "<b>World</b>") {
		t.Errorf("HTML = %q", parsed.HTML)
	}
}

func TestParseEmptyMessage(t *testing.T) {
	if _, err := mailmime.Parse(nil, mailmime.Options{}); err == nil {
		t.Fatal("expected an error for an empty message")
	}
}
