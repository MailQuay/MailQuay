package config

import "testing"

func TestLoadReadsNetworkEnvironment(t *testing.T) {
	t.Setenv("MAILQUAY_DATABASE_URL", "postgres://mailquay:mailquay@127.0.0.1:5432/mailquay")
	t.Setenv("MAILQUAY_SECRET_KEY", "secret")
	t.Setenv("MAILQUAY_PUBLIC_URL", "https://mail.example.com/")
	t.Setenv("MAILQUAY_CORS_ORIGINS", "https://app.example.com, http://localhost:5173")
	t.Setenv("MAILQUAY_TRUSTED_PROXIES", "10.0.0.0/8, 172.16.0.0/12")
	t.Setenv("MAILQUAY_INSECURE_ALLOW", "mail.lan, 10.1.0.0/16")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PublicURL != "https://mail.example.com" {
		t.Errorf("PublicURL = %q", cfg.PublicURL)
	}
	if len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[0] != "https://app.example.com" ||
		cfg.CORSOrigins[1] != "http://localhost:5173" {
		t.Errorf("CORSOrigins = %v", cfg.CORSOrigins)
	}
	if len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[0] != "10.0.0.0/8" {
		t.Errorf("TrustedProxies = %v", cfg.TrustedProxies)
	}
	if len(cfg.InsecureAllow) != 2 || cfg.InsecureAllow[0] != "mail.lan" {
		t.Errorf("InsecureAllow = %v", cfg.InsecureAllow)
	}
}

func TestLoadRejectsInvalidCORSOrigin(t *testing.T) {
	t.Setenv("MAILQUAY_DATABASE_URL", "postgres://mailquay:mailquay@127.0.0.1:5432/mailquay")
	t.Setenv("MAILQUAY_SECRET_KEY", "secret")
	t.Setenv("MAILQUAY_CORS_ORIGINS", "https://app.example.com/path")

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid CORS origin to fail")
	}
}
