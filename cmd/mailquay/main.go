package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/app"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/store"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe()
	case "migrate":
		err = runMigrate(os.Args[2:])
	case "search":
		err = runSearch(os.Args[2:])
	case "healthcheck":
		err = runHealthcheck()
	case "version":
		fmt.Printf("mailquay %s (%s, %s)\n", version, commit, buildTime)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: mailquay <command>

commands:
  serve                     start the HTTP server and background workers
  migrate up|down [n]|status
  search reindex [--all] [--since RFC3339] [--mailbox UUID]
  healthcheck               probe /healthz
  version`)
}

func runServe() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	return application.Run(ctx)
}

func runMigrate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("migrate requires up, down or status")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch args[0] {
	case "up":
		return store.Migrate(ctx, cfg.DatabaseURL)
	case "down":
		steps := 1
		if len(args) > 1 {
			if parsed, err := strconv.Atoi(args[1]); err == nil && parsed > 0 {
				steps = parsed
			}
		}
		return store.MigrateDown(ctx, cfg.DatabaseURL, steps)
	case "status":
		return store.MigrationStatus(ctx, cfg.DatabaseURL)
	default:
		return fmt.Errorf("unknown migrate command %q", args[0])
	}
}

func runSearch(args []string) error {
	if len(args) == 0 || args[0] != "reindex" {
		return fmt.Errorf("search requires the reindex command")
	}
	flags := flag.NewFlagSet("search reindex", flag.ContinueOnError)
	all := flags.Bool("all", false, "rebuild every message and archive")
	sinceRaw := flags.String("since", "", "only rebuild messages received after this RFC3339 time")
	mailboxRaw := flags.String("mailbox", "", "only rebuild messages in this mailbox UUID")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	var since time.Time
	if *sinceRaw != "" {
		parsed, err := time.Parse(time.RFC3339, *sinceRaw)
		if err != nil {
			return fmt.Errorf("invalid --since: %w", err)
		}
		since = parsed
	}
	var mailboxID *uuid.UUID
	if *mailboxRaw != "" {
		parsed, err := uuid.Parse(*mailboxRaw)
		if err != nil {
			return fmt.Errorf("invalid --mailbox: %w", err)
		}
		mailboxID = &parsed
	}
	if !*all && since.IsZero() && mailboxID == nil {
		return fmt.Errorf("reindex requires --all, --since or --mailbox")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	application, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer application.Close()
	return application.Reindex(ctx, *all, since, mailboxID)
}

func runHealthcheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("parse listen address: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned %d", resp.StatusCode)
	}
	return nil
}

var _ = slog.LevelInfo
var _ = platform.NewID
var _ = strings.TrimSpace
