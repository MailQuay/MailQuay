package emclient

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/pbkdf2"

	_ "modernc.org/sqlite"
)

type testConfig struct {
	Type  string
	Value map[string]any
}

func encryptForTest(t *testing.T, plaintext, password string) string {
	t.Helper()
	key := pbkdf2.Key([]byte(password), []byte(keySalt), keyIterations, keyLength, sha1.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	plain := append([]byte(plaintext), bytesRepeat(byte(pad), pad)...)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, plain)

	envelope := make([]byte, 4+len(iv)+len(out))
	binary.LittleEndian.PutUint32(envelope[:4], uint32(len(iv)))
	copy(envelope[4:], iv)
	copy(envelope[4+len(iv):], out)
	return base64.StdEncoding.EncodeToString(envelope)
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func cred(kind, username, secret string) map[string]any {
	value := map[string]any{"Type": kind, "Username": username, "SaveSecret": secret != ""}
	if secret != "" {
		value["Secret"] = secret
	}
	return value
}

func writeDB(t *testing.T, configs []testConfig) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "accounts.dat"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE AccountsJson (key TEXT PRIMARY KEY, value)`); err != nil {
		t.Fatal(err)
	}
	for i, cfg := range configs {
		raw, err := json.Marshal(map[string]any{
			"Type":      cfg.Type,
			"TypeValue": cfg.Value,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO AccountsJson (key, value) VALUES (?, ?)`, i, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestReadAccountsPassword(t *testing.T) {
	password := encryptForTest(t, "imap-secret", fallbackKey)
	smtpPassword := encryptForTest(t, "smtp-secret", fallbackKey)
	dir := writeDB(t, []testConfig{
		{
			Type: "MailClient.Protocols.Imap.ImapAccountConfiguration, MailClient.Protocols.Imap",
			Value: map[string]any{
				"AccountName":             "User@Example.com (Work)",
				"Host":                    "imap.example.com",
				"Port":                    993,
				"SSL":                     "UseTlsOnSpecialPort",
				"BindingConfigurationUID": "binding-1",
				"ServerType":              "Other",
				"Enabled":                 true,
				"Credentials":             cred("Password", "user@example.com", password),
			},
		},
		{
			Type: "MailClient.Protocols.Smtp.SmtpAccountConfiguration, MailClient.Protocols.Smtp",
			Value: map[string]any{
				"AccountName":             "User@Example.com (Work)",
				"Host":                    "smtp.example.com",
				"Port":                    587,
				"SSL":                     "UseTlsAlways",
				"BindingConfigurationUID": "binding-1",
				"Credentials":             cred("Password", "smtp-user@example.com", smtpPassword),
			},
		},
	})

	accounts, err := ReadAccounts(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	got := accounts[0]
	if got.Email != "user@example.com" {
		t.Errorf("email = %q", got.Email)
	}
	if got.IMAPAddr != "imap.example.com:993" || got.IMAPSecurity != "implicit_tls" {
		t.Errorf("imap = %q %q", got.IMAPAddr, got.IMAPSecurity)
	}
	if got.SMTPAddr != "smtp.example.com:587" || got.SMTPSecurity != "starttls" {
		t.Errorf("smtp = %q %q", got.SMTPAddr, got.SMTPSecurity)
	}
	if got.Password != "imap-secret" {
		t.Errorf("password was not decrypted")
	}
	if got.SMTPUsername != "smtp-user@example.com" || got.SMTPPassword != "smtp-secret" {
		t.Errorf("smtp credentials were not decrypted")
	}
	if got.SkipCode != "" {
		t.Errorf("unexpected skip: %s", got.SkipCode)
	}
}

func TestReadAccountsMasterPassword(t *testing.T) {
	password := encryptForTest(t, "secret", "startup-password")
	dir := writeDB(t, []testConfig{{
		Type: "MailClient.Protocols.Imap.ImapAccountConfiguration, MailClient.Protocols.Imap",
		Value: map[string]any{
			"AccountName":             "user@example.com",
			"Host":                    "imap.example.com",
			"Port":                    993,
			"SSL":                     "UseTlsOnSpecialPort",
			"BindingConfigurationUID": "binding-1",
			"Credentials":             cred("Password", "user@example.com", password),
		},
	}})

	accounts, err := ReadAccounts(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].SkipCode != "decrypt_failed" {
		t.Fatalf("skip code = %q, want decrypt_failed", accounts[0].SkipCode)
	}

	accounts, err = ReadAccounts(context.Background(), dir, Options{MasterPassword: "startup-password"})
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].SkipCode != "" || accounts[0].Password != "secret" {
		t.Fatalf("master password was not used: skip=%q", accounts[0].SkipCode)
	}
}

func TestReadAccountsOAuth(t *testing.T) {
	token := encryptForTest(t, "refresh-token", fallbackKey)
	dir := writeDB(t, []testConfig{
		{
			Type: "MailClient.Protocols.Imap.ImapAccountConfiguration, MailClient.Protocols.Imap",
			Value: map[string]any{
				"AccountName":             "user@gmail.com",
				"Host":                    "imap.gmail.com",
				"Port":                    993,
				"SSL":                     "UseTlsOnSpecialPort",
				"BindingConfigurationUID": "binding-1",
				"ServerType":              "Gmail",
				"Credentials":             map[string]any{"Type": "Password", "Username": "user@gmail.com", "SaveSecret": false},
			},
		},
		{
			Type: "MailClient.Protocols.Smtp.SmtpAccountConfiguration, MailClient.Protocols.Smtp",
			Value: map[string]any{
				"AccountName":             "user@gmail.com",
				"Host":                    "smtp.gmail.com",
				"Port":                    587,
				"SSL":                     "UseTlsAlways",
				"BindingConfigurationUID": "binding-1",
				"Credentials":             map[string]any{"Type": "Password", "Username": "user@gmail.com", "SaveSecret": false},
			},
		},
		{
			// eM Client stores the OAuth token in a separate record that has
			// no binding UID and is associated by account name.
			Type: "MailClient.Accounts.Mail.MailAccountConfiguration, MailClient.Accounts",
			Value: map[string]any{
				"AccountName": "user@gmail.com",
				"Credentials": cred("OAuthRefreshToken", "user@gmail.com", token),
			},
		},
	})

	accounts, err := ReadAccounts(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	got := accounts[0]
	if got.AuthType != "oauth2" {
		t.Errorf("auth = %q", got.AuthType)
	}
	if got.Password != "" {
		t.Error("OAuth account must not expose a password")
	}
}

func TestReadAccountsMissingPassword(t *testing.T) {
	dir := writeDB(t, []testConfig{{
		Type: "MailClient.Protocols.Imap.ImapAccountConfiguration, MailClient.Protocols.Imap",
		Value: map[string]any{
			"AccountName":             "user@example.com",
			"Host":                    "imap.example.com",
			"Port":                    993,
			"SSL":                     "UseTlsOnSpecialPort",
			"BindingConfigurationUID": "binding-1",
			"Credentials":             map[string]any{"Type": "Password", "Username": "user@example.com", "SaveSecret": false},
		},
	}})

	accounts, err := ReadAccounts(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].SkipCode != "no_password" {
		t.Fatalf("skip code = %q, want no_password", accounts[0].SkipCode)
	}
}

func TestReadAccountsRequiresAccountsDat(t *testing.T) {
	dir := t.TempDir()
	if _, err := os.Stat(filepath.Join(dir, "accounts.dat")); err == nil {
		t.Fatal("fixture unexpectedly created accounts.dat")
	}
	if _, err := ReadAccounts(context.Background(), dir, Options{}); err == nil {
		t.Fatal("expected an error for a missing accounts.dat")
	}
}

func TestReadAccountsPrefersCredentialForAccountUsername(t *testing.T) {
	accountPassword := encryptForTest(t, "account-secret", fallbackKey)
	smtpPassword := encryptForTest(t, "smtp-secret", fallbackKey)
	dir := writeDB(t, []testConfig{
		{
			Type: "MailClient.Protocols.Imap.ImapAccountConfiguration, MailClient.Protocols.Imap",
			Value: map[string]any{
				"AccountName":             "user@example.com",
				"Host":                    "imap.example.com",
				"Port":                    993,
				"SSL":                     "UseTlsOnSpecialPort",
				"BindingConfigurationUID": "binding-1",
				"Credentials":             map[string]any{"Type": "Password", "Username": "user@example.com", "SaveSecret": false},
			},
		},
		{
			Type: "MailClient.Protocols.Smtp.SmtpAccountConfiguration, MailClient.Protocols.Smtp",
			Value: map[string]any{
				"AccountName":             "user@example.com",
				"Host":                    "smtp.example.com",
				"Port":                    587,
				"SSL":                     "UseTlsAlways",
				"BindingConfigurationUID": "binding-1",
				"Credentials":             cred("Password", "smtp-user@example.com", smtpPassword),
			},
		},
		{
			Type: "MailClient.Accounts.Mail.MailAccountConfiguration, MailClient.Accounts",
			Value: map[string]any{
				"AccountName": "user@example.com",
				"Credentials": cred("Password", "user@example.com", accountPassword),
			},
		},
	})

	accounts, err := ReadAccounts(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := accounts[0]
	if got.Password != "account-secret" {
		t.Errorf("account password came from the wrong credential")
	}
	if got.SMTPUsername != "smtp-user@example.com" || got.SMTPPassword != "smtp-secret" {
		t.Errorf("smtp credentials = %q / %q", got.SMTPUsername, got.SMTPPassword)
	}
}
