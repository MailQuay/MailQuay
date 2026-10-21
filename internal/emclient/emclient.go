// Package emclient reads mailbox accounts stored by the eM Client desktop
// application.
//
// eM Client keeps account configurations in a SQLite database named
// accounts.dat. Passwords are encrypted with AES-256-CBC using a key derived
// from the optional startup password, or from a fixed fallback key when no
// startup password is configured.
package emclient

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"

	_ "modernc.org/sqlite"
)

const (
	// fallbackKey is used by eM Client when no startup password is set.
	fallbackKey = "72905C47-F4FD-4CF7-A489-4E8121A155BD"
	// keySalt and keyIterations match the AESEncryptor implementation in eM
	// Client. The salt is a constant, not a per-message value.
	keySalt       = "o6806642kbM7c5"
	keyIterations = 1000
	keyLength     = 32
)

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// Options controls how the eM Client database is read.
type Options struct {
	// MasterPassword is the eM Client startup password. Leave empty when the
	// desktop application is not password protected.
	MasterPassword string
}

// Account is a mailbox account extracted from eM Client.
type Account struct {
	Email           string
	Name            string
	Provider        string
	Username        string
	Password        string
	AuthType        string
	ReceiveProtocol string
	IMAPAddr        string
	IMAPSecurity    string
	POP3Addr        string
	POP3Security    string
	SMTPAddr        string
	SMTPSecurity    string
	SMTPUsername    string
	SMTPPassword    string
	SyncEnabled     bool
	SkipCode        string
}

type rawEnvelope struct {
	Type      string          `json:"Type"`
	TypeValue json.RawMessage `json:"TypeValue"`
}

type rawValue struct {
	AccountName             string   `json:"AccountName"`
	Host                    string   `json:"Host"`
	Port                    int      `json:"Port"`
	SSL                     string   `json:"SSL"`
	BindingConfigurationUID string   `json:"BindingConfigurationUID"`
	ServerType              string   `json:"ServerType"`
	Enabled                 *bool    `json:"Enabled"`
	Credentials             *rawCred `json:"Credentials"`
}

type rawCred struct {
	Type     string  `json:"Type"`
	Username string  `json:"Username"`
	Secret   *string `json:"Secret"`
}

type group struct {
	imap  *rawValue
	pop3  *rawValue
	smtp  *rawValue
	extra []*rawValue
}

type parsedConfig struct {
	kind string
	cfg  rawValue
}

// ReadAccounts opens the eM Client data directory and returns one entry per
// configured mailbox account. Accounts that cannot be represented by MailQuay
// are returned with SkipCode set instead of failing the whole import.
func ReadAccounts(ctx context.Context, dataDir string, opts Options) ([]Account, error) {
	tmp, cleanup, err := snapshot(dataDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	db, err := sql.Open("sqlite", "file:"+filepath.Join(tmp, "accounts.dat")+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open accounts database: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT value FROM AccountsJson`)
	if err != nil {
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	defer rows.Close()

	var configs []parsedConfig
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan account row: %w", err)
		}
		var env rawEnvelope
		if err := json.Unmarshal(value, &env); err != nil {
			return nil, fmt.Errorf("decode account row: %w", err)
		}
		var cfg rawValue
		if err := json.Unmarshal(env.TypeValue, &cfg); err != nil {
			return nil, fmt.Errorf("decode account configuration: %w", err)
		}
		kind := ""
		switch {
		case strings.Contains(env.Type, "Imap.ImapAccountConfiguration"):
			kind = "imap"
		case strings.Contains(env.Type, "Pop3.Pop3AccountConfiguration"):
			kind = "pop3"
		case strings.Contains(env.Type, "Smtp.SmtpAccountConfiguration"):
			kind = "smtp"
		}
		configs = append(configs, parsedConfig{kind: kind, cfg: cfg})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate account rows: %w", err)
	}

	groups := make(map[string]*group)
	extras := make(map[string][]*rawValue)
	for i := range configs {
		cfg := &configs[i].cfg
		switch configs[i].kind {
		case "imap", "pop3", "smtp":
			key := cfg.BindingConfigurationUID
			if key == "" {
				key = strings.ToLower(cfg.AccountName)
			}
			if key == "" {
				continue
			}
			g := groups[key]
			if g == nil {
				g = &group{}
				groups[key] = g
			}
			switch configs[i].kind {
			case "imap":
				g.imap = cfg
			case "pop3":
				g.pop3 = cfg
			case "smtp":
				g.smtp = cfg
			}
		default:
			if email, ok := extractEmail(cfg.AccountName); ok {
				extras[email] = append(extras[email], cfg)
			}
		}
	}

	accounts := make([]Account, 0, len(groups))
	for _, g := range groups {
		if g.imap == nil && g.pop3 == nil {
			continue
		}
		if email, ok := groupEmail(g); ok {
			g.extra = append(g.extra, extras[email]...)
		}
		accounts = append(accounts, buildAccount(g, opts))
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].Email < accounts[j].Email
	})
	return accounts, nil
}

func groupEmail(g *group) (string, bool) {
	for _, cfg := range []*rawValue{g.imap, g.pop3, g.smtp} {
		if cfg == nil {
			continue
		}
		if email, ok := extractEmail(cfg.AccountName); ok {
			return email, true
		}
	}
	return "", false
}

func buildAccount(g *group, opts Options) Account {
	primary := g.imap
	if primary == nil {
		primary = g.pop3
	}
	if primary == nil {
		return Account{SkipCode: "invalid"}
	}

	email, ok := extractEmail(primary.AccountName)
	if !ok {
		return Account{SkipCode: "invalid"}
	}

	acct := Account{
		Email:           email,
		Name:            email,
		Provider:        providerFor(email),
		Username:        usernameFor(g),
		AuthType:        "password",
		ReceiveProtocol: "imap",
		SyncEnabled:     primary.Enabled == nil || *primary.Enabled,
	}
	if g.imap != nil {
		acct.IMAPAddr = addr(g.imap.Host, g.imap.Port)
		acct.IMAPSecurity = security(g.imap.SSL, g.imap.Port)
	} else {
		acct.ReceiveProtocol = "pop3"
		acct.POP3Addr = addr(g.pop3.Host, g.pop3.Port)
		acct.POP3Security = security(g.pop3.SSL, g.pop3.Port)
	}

	if g.smtp != nil {
		acct.SMTPAddr = addr(g.smtp.Host, g.smtp.Port)
		acct.SMTPSecurity = security(g.smtp.SSL, g.smtp.Port)
		if g.smtp.Credentials != nil && strings.TrimSpace(g.smtp.Credentials.Username) != "" {
			acct.SMTPUsername = strings.TrimSpace(g.smtp.Credentials.Username)
		}
	}

	if acct.Username == "" {
		return withSkip(acct, "invalid")
	}
	if secret := passwordCredential(g, acct.Username); secret != nil {
		password, err := decryptSecret(*secret.Secret, opts.MasterPassword)
		if err != nil {
			return withSkip(acct, "decrypt_failed")
		}
		acct.Password = password
	} else if oauthCredential(g) != nil {
		acct.AuthType = "oauth2"
		return acct
	} else {
		return withSkip(acct, "no_password")
	}

	if acct.SMTPAddr != "" {
		switch {
		case g.smtp != nil && g.smtp.Credentials != nil && g.smtp.Credentials.Secret != nil && *g.smtp.Credentials.Secret != "":
			smtpPassword, err := decryptSecret(*g.smtp.Credentials.Secret, opts.MasterPassword)
			if err != nil {
				return withSkip(acct, "decrypt_failed")
			}
			acct.SMTPPassword = smtpPassword
		case acct.SMTPUsername != "" && acct.SMTPUsername != acct.Username:
			return withSkip(acct, "no_password")
		}
	}
	return acct
}

func withSkip(acct Account, code string) Account {
	acct.SkipCode = code
	return acct
}

func passwordCredential(g *group, username string) *rawCred {
	configs := credentialConfigs(g)
	for _, cfg := range configs {
		if cfg.Credentials.Type == "Password" && cfg.Credentials.Secret != nil && *cfg.Credentials.Secret != "" &&
			strings.EqualFold(strings.TrimSpace(cfg.Credentials.Username), username) {
			return cfg.Credentials
		}
	}
	for _, cfg := range configs {
		if cfg.Credentials.Type == "Password" && cfg.Credentials.Secret != nil && *cfg.Credentials.Secret != "" {
			return cfg.Credentials
		}
	}
	return nil
}

func oauthCredential(g *group) *rawCred {
	for _, cfg := range credentialConfigs(g) {
		if cfg.Credentials.Type == "OAuthRefreshToken" && cfg.Credentials.Secret != nil && *cfg.Credentials.Secret != "" {
			return cfg.Credentials
		}
	}
	return nil
}

func credentialConfigs(g *group) []*rawValue {
	var configs []*rawValue
	for _, cfg := range append([]*rawValue{g.imap, g.pop3, g.smtp}, g.extra...) {
		if cfg != nil && cfg.Credentials != nil {
			configs = append(configs, cfg)
		}
	}
	return configs
}

func usernameFor(g *group) string {
	for _, cfg := range append([]*rawValue{g.imap, g.pop3, g.smtp}, g.extra...) {
		if cfg != nil && cfg.Credentials != nil && strings.TrimSpace(cfg.Credentials.Username) != "" {
			return strings.TrimSpace(cfg.Credentials.Username)
		}
	}
	return ""
}

func extractEmail(raw string) (string, bool) {
	match := emailPattern.FindString(raw)
	if match == "" {
		return "", false
	}
	return strings.ToLower(match), true
}

func addr(host string, port int) string {
	host = strings.TrimSpace(host)
	if host == "" || port < 1 || port > 65535 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func security(mode string, port int) string {
	switch mode {
	case "UseTlsOnSpecialPort":
		return "implicit_tls"
	case "UseTlsAlways", "UseTlsIfAvailable":
		return "starttls"
	case "None":
		return "none"
	}
	switch port {
	case 993, 995, 465:
		return "implicit_tls"
	case 143, 110, 587:
		return "starttls"
	}
	return "none"
}

func providerFor(email string) string {
	domain := email[strings.LastIndex(email, "@")+1:]
	switch domain {
	case "gmail.com", "googlemail.com":
		return "gmail"
	case "outlook.com", "hotmail.com", "live.com", "live.cn", "msn.com":
		return "outlook"
	case "qq.com", "foxmail.com":
		return "qq"
	case "163.com", "126.com", "yeah.net":
		return "netease"
	case "aliyun.com":
		return "aliyun"
	case "yahoo.com":
		return "yahoo"
	case "icloud.com", "mac.com":
		return "icloud"
	}
	return "custom"
}

// decryptSecret decodes the eM Client secret envelope:
// uint32 little-endian IV length, IV, AES-CBC ciphertext with PKCS#7 padding.
func decryptSecret(encoded, masterPassword string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	if len(raw) < 4 {
		return "", errors.New("secret envelope is too short")
	}
	ivLen := int(binary.LittleEndian.Uint32(raw[:4]))
	if ivLen <= 0 || ivLen > 32 || 4+ivLen > len(raw) {
		return "", errors.New("secret envelope has an invalid IV length")
	}
	iv := raw[4 : 4+ivLen]
	ciphertext := raw[4+ivLen:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", errors.New("secret ciphertext is not block aligned")
	}

	password := masterPassword
	if password == "" {
		password = fallbackKey
	}
	key := pbkdf2.Key([]byte(password), []byte(keySalt), keyIterations, keyLength, sha1.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)
	plain, err = unpad(plain)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty padded data")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > aes.BlockSize || n > len(data) {
		return nil, errors.New("invalid padding")
	}
	for _, b := range data[len(data)-n:] {
		if int(b) != n {
			return nil, errors.New("invalid padding")
		}
	}
	return data[:len(data)-n], nil
}

// snapshot copies the database and its WAL files so a running eM Client does
// not need to be stopped and no locks are taken on the original files.
func snapshot(dataDir string) (string, func(), error) {
	src := filepath.Join(dataDir, "accounts.dat")
	info, err := os.Stat(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("accounts.dat not found in %s", dataDir)
		}
		return "", nil, fmt.Errorf("stat accounts.dat: %w", err)
	}
	if info.IsDir() {
		return "", nil, fmt.Errorf("%s is a directory, expected accounts.dat", src)
	}

	tmp, err := os.MkdirTemp("", "mailquay-emclient-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from := src + suffix
		if _, err := os.Stat(from); err != nil {
			continue
		}
		if err := copyFile(from, filepath.Join(tmp, "accounts.dat"+suffix)); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return tmp, cleanup, nil
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("open %s: %w", from, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", to, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("copy %s: %w", from, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close %s: %w", to, err)
	}
	return nil
}
