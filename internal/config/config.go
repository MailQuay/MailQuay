package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Config is the validated runtime configuration.
type Config struct {
	ListenAddr     string
	PublicURL      string
	DatabaseURL    string
	DataDir        string
	SearchPath     string
	SecretKey      string
	LogLevel       string
	LogFormat      string
	TrustedProxies []string
	CORSOrigins    []string
	InsecureAllow  []string
	SetupToken     string
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:  env("MAILQUAY_LISTEN_ADDR", ":8080"),
		PublicURL:   strings.TrimRight(env("MAILQUAY_PUBLIC_URL", ""), "/"),
		DatabaseURL: env("MAILQUAY_DATABASE_URL", ""),
		DataDir:     env("MAILQUAY_DATA_DIR", "./data"),
		SecretKey:   env("MAILQUAY_SECRET_KEY", ""),
		LogLevel:    env("MAILQUAY_LOG_LEVEL", "info"),
		LogFormat:   env("MAILQUAY_LOG_FORMAT", "text"),
		SetupToken:  env("MAILQUAY_SETUP_TOKEN", ""),
	}
	cfg.SearchPath = env("MAILQUAY_SEARCH_PATH", cfg.DataDir+"/search")
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("MAILQUAY_DATABASE_URL is required")
	}
	if cfg.SecretKey == "" {
		return nil, fmt.Errorf("MAILQUAY_SECRET_KEY is required")
	}
	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || !u.IsAbs() || u.Host == "" {
			return nil, fmt.Errorf("MAILQUAY_PUBLIC_URL must be an absolute URL")
		}
	}
	cfg.TrustedProxies = splitList(env("MAILQUAY_TRUSTED_PROXIES", ""))
	for _, raw := range cfg.TrustedProxies {
		if _, _, err := net.ParseCIDR(raw); err != nil {
			return nil, fmt.Errorf("MAILQUAY_TRUSTED_PROXIES: invalid CIDR %q", raw)
		}
	}
	for _, raw := range splitList(env("MAILQUAY_CORS_ORIGINS", "")) {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("MAILQUAY_CORS_ORIGINS: invalid origin %q", raw)
		}
		cfg.CORSOrigins = append(cfg.CORSOrigins, u.Scheme+"://"+u.Host)
	}
	cfg.InsecureAllow = splitList(env("MAILQUAY_INSECURE_ALLOW", ""))
	return cfg, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
