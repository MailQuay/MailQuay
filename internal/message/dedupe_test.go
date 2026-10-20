package message

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDedupeKeyFallbacks(t *testing.T) {
	uid := int64(42)
	if got := DedupeKey("gm-1", "<a@b>", "hash", "mb", &uid); got != "p:gm-1" {
		t.Errorf("provider id priority: %q", got)
	}
	if got := DedupeKey("", "<A@B>", "", "mb", &uid); got != "m:a@b" {
		t.Errorf("message id priority: %q", got)
	}
	if got := DedupeKey("", "", "abcdef", "mb", &uid); got != "h:abcdef" {
		t.Errorf("hash fallback: %q", got)
	}
	if got := DedupeKey("", "", "", "mb", &uid); got != "u:mb:42" {
		t.Errorf("uid fallback: %q", got)
	}
}

func TestSubjectThreadKey(t *testing.T) {
	cases := map[string]string{
		"Re: Hello":         "hello",
		"Fwd: Re: Hello":    "hello",
		"FW:Project Update": "project update",
		"Plain":             "plain",
	}
	for in, want := range cases {
		if got := SubjectThreadKey(in); got != want {
			t.Errorf("SubjectThreadKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	id := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	encoded := EncodeCursor(now, id)
	gotTime, gotID, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.Equal(now) || gotID != id {
		t.Fatalf("cursor mismatch: %v %v", gotTime, gotID)
	}
	if _, _, err := DecodeCursor("!!!not-base64"); err == nil {
		t.Fatal("expected invalid cursor error")
	}
}
