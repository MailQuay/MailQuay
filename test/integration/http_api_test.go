//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/app"
	"github.com/mailquay/mailquay/internal/auth"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/store"
)

func TestHTTPAuthAndAccountFlow(t *testing.T) {
	dsn := os.Getenv("MAILQUAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("MAILQUAY_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resetSchema(t, ctx, dsn)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := &config.Config{
		DatabaseURL: dsn, DataDir: dataDir, SearchPath: filepath.Join(dataDir, "search"),
		SecretKey: mustKey(t), ListenAddr: ":0", SetupToken: "setup-token",
	}
	application, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	server := httptest.NewServer(application.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := server.URL + "/api/v1"

	status := getJSON(t, client, base+"/auth/me")
	if status["needsSetup"] != true {
		t.Fatalf("needsSetup = %v", status["needsSetup"])
	}
	resp := postJSON(t, client, base+"/auth/setup", map[string]any{
		"token": "setup-token", "username": "owner", "password": "supersecret1",
	}, "")
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup status %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	csrf := csrfFromJar(t, jar, server.URL)
	me := getJSON(t, client, base+"/auth/me")
	user, _ := me["user"].(map[string]any)
	if user == nil || user["username"] != "owner" {
		t.Fatalf("me = %+v", me)
	}
	settings := getJSON(t, client, base+"/settings")
	if settings["sendRetryLimit"] != float64(3) {
		t.Fatalf("default retry limit = %v", settings["sendRetryLimit"])
	}
	resp = sendJSON(t, client, http.MethodPatch, base+"/settings", map[string]any{
		"sendRetryLimit": 5, "language": "zh-CN",
	}, csrf)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("settings patch %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	settings = getJSON(t, client, base+"/settings")
	if settings["sendRetryLimit"] != float64(5) || settings["language"] != "zh-CN" {
		t.Fatalf("settings not persisted: %+v", settings)
	}
	resp = sendJSON(t, client, http.MethodPatch, base+"/settings", map[string]any{
		"publicUrl": "https://mail.example.com", "sessionTtlHours": 72,
		"syncIntervalSeconds": 120, "pop3IntervalSeconds": 90,
		"bodyPrefetchMaxBytes": 1048576, "maxAttachmentBytes": 10485760,
		"corsOrigins":    []string{"https://app.example.com"},
		"trustedProxies": []string{"10.0.0.0/8"}, "insecureAllow": []string{"mail.lan"},
		"googleClientId": "google-id", "googleClientSecret": "google-secret",
		"logLevel": "debug",
	}, csrf)
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("server settings patch %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
	settings = getJSON(t, client, base+"/settings")
	if settings["publicUrl"] != "https://mail.example.com" || settings["sessionTtlHours"] != float64(72) ||
		settings["logLevel"] != "debug" || settings["googleSecretSet"] != true {
		t.Fatalf("runtime settings not persisted: %+v", settings)
	}
	oauth, _ := settings["oauth"].(map[string]any)
	_ = oauth
	loginResp := sendJSON(t, client, http.MethodPost, base+"/auth/login", map[string]any{
		"username": "owner", "password": "supersecret1",
	}, "")
	setCookie := strings.Join(loginResp.Header.Values("Set-Cookie"), "\n")
	loginResp.Body.Close()
	if !strings.Contains(setCookie, "SameSite=None") || !strings.Contains(setCookie, "Secure") {
		t.Fatalf("cross-origin cookie policy not applied: %s", setCookie)
	}
	csrf = csrfFromJar(t, jar, server.URL)
	if _, leaked := settings["googleClientSecret"]; leaked {
		t.Fatal("OAuth secret leaked in settings response")
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var stored string
	if err := conn.QueryRow(ctx, "SELECT value::text FROM settings WHERE key = 'google_client_secret'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "google-secret") {
		t.Fatal("OAuth secret stored in plaintext")
	}
	resp = sendJSON(t, client, http.MethodPost, base+"/accounts", map[string]any{
		"name": "Test", "email": "api@example.com", "username": "api", "authType": "password",
		"password": "secret", "receiveProtocol": "imap", "imapAddr": "127.0.0.1:143",
		"imapSecurity": "starttls", "smtpAddr": "127.0.0.1:25", "smtpSecurity": "none",
		"syncEnabled": true, "initialSyncRange": "all", "downloadScope": "full_no_attachments",
	}, csrf)
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create account %d: %s", resp.StatusCode, body)
	}
	var account map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&account); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if account["hasPassword"] != true || account["imapAddr"] != "127.0.0.1:143" {
		t.Fatalf("account = %+v", account)
	}
	if _, ok := account["password"]; ok {
		t.Fatal("password leaked in response")
	}
	tree := getJSON(t, client, base+"/mailboxes")
	if _, ok := tree["accounts"].([]any); !ok {
		t.Fatalf("tree = %+v", tree)
	}
}

func mustKey(t *testing.T) string {
	t.Helper()
	return "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
}

func getJSON(t *testing.T, client *http.Client, url string) map[string]any {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return out
}

func postJSON(t *testing.T, client *http.Client, url string, body map[string]any, csrf string) *http.Response {
	t.Helper()
	return sendJSON(t, client, http.MethodPost, url, body, csrf)
}

func sendJSON(t *testing.T, client *http.Client, method, url string, body map[string]any, csrf string) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func csrfFromJar(t *testing.T, jar http.CookieJar, serverURL string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, serverURL, nil)
	for _, cookie := range jar.Cookies(req.URL) {
		if cookie.Name == auth.CSRFCookie {
			return cookie.Value
		}
	}
	t.Fatal("csrf cookie missing")
	return ""
}
