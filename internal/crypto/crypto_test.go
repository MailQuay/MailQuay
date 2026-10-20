package crypto

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptAAD(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := c.EncryptString("s3cret", []byte("account:a"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := c.DecryptString(ciphertext, []byte("account:a"))
	if err != nil || plain != "s3cret" {
		t.Fatalf("decrypt = %q, %v", plain, err)
	}
	if _, err := c.DecryptString(ciphertext, []byte("account:b")); err == nil {
		t.Fatal("expected AAD mismatch")
	}
	second, _ := c.EncryptString("s3cret", []byte("account:a"))
	if bytes.Equal(ciphertext, second) {
		t.Fatal("nonce was reused")
	}
}

func TestKeyLength(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("expected short key rejection")
	}
}
