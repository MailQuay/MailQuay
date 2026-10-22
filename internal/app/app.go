package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/auth"
	"github.com/mailquay/mailquay/internal/compose"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/crypto"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/httpapi"
	"github.com/mailquay/mailquay/internal/identity"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/mailsession"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/netpace"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/search"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
	syncservice "github.com/mailquay/mailquay/internal/sync"
)

// dialInterval spaces connection attempts to the same mail host. Providers
// throttle and risk-control clients by connection rate, so background work is
// paced by the remote endpoint instead of local concurrency alone.
const dialInterval = 2 * time.Second

// sessionIdleTimeout closes an account's idle IMAP connection so it does not
// hold a server slot between syncs.
const sessionIdleTimeout = 3 * time.Minute

// App is the assembled server process.
type App struct {
	cfg       *config.Config
	logger    *slog.Logger
	store     *store.Store
	blobs     *storage.Store
	index     *search.Index
	hub       *events.Hub
	settings  *settings.Service
	sync      *syncservice.Service
	compose   *compose.Service
	sessions  *mailsession.Pool
	messages  *message.Service
	accounts  *account.Service
	server    *http.Server
	indexer   *Indexer
	closeOnce sync.Once
}

// New assembles all dependencies.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	logger, levelVar := platform.NewLoggerLevel(cfg.LogLevel, cfg.LogFormat)
	cipher, err := crypto.NewFromBase64(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	blobs, err := storage.New(cfg.DataDir)
	if err != nil {
		st.Close()
		return nil, err
	}
	index, err := search.Open(cfg.SearchPath)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("open search index: %w", err)
	}
	resources := platform.DetectResources()
	logger.Info("resource limits detected", "cpus", resources.CPUs, "memory_bytes", resources.MemoryBytes,
		"cgroup_version", resources.CgroupVersion, "cpu_source", resources.CPUSource, "memory_source", resources.MemorySource)
	settingsSvc := settings.NewService(st, resources, cipher, settings.Bootstrap{
		LogLevel: cfg.LogLevel,
	})
	settingsSvc.OnChange = func(values settings.Values) {
		levelVar.Set(platform.ParseLevel(values.LogLevel))
		index.SetQueryLimit(settings.EffectiveConcurrency(values, settingsSvc.Resources()).SearchConcurrency)
	}
	snap, err := settingsSvc.Get(ctx)
	if err != nil {
		index.Close()
		st.Close()
		return nil, err
	}
	index.SetQueryLimit(snap.Effective.SearchConcurrency)
	logger.Info("concurrency", "sync", snap.Effective.SyncConcurrency, "fetch", snap.Effective.FetchConcurrency,
		"search", snap.Effective.SearchConcurrency, "index_batch", snap.Effective.IndexBatchSize)

	hub := events.NewHub()
	pacer := netpace.New(dialInterval)
	sessions := mailsession.NewPool(sessionIdleTimeout)
	accounts := account.NewService(st, cipher, logger)
	accounts.SetSettings(settingsSvc)
	ids := identity.NewService(st)
	mailboxes := mailbox.NewService(st)
	messages := message.NewService(st, hub, blobs, logger)
	if removed, err := messages.CleanupMovedDuplicates(ctx); err != nil {
		logger.Warn("clean moved duplicates", "error", err)
	} else if removed > 0 {
		logger.Info("removed duplicate moved messages", "count", removed)
	}
	composeSvc := compose.NewService(st, accounts, ids, settingsSvc, messages, blobs, hub, logger, pacer, sessions)
	syncSvc := syncservice.NewService(st, cfg, logger, hub, blobs, accounts, mailboxes, messages, settingsSvc, pacer, sessions)

	indexer := NewIndexer(st, index, blobs, hub, settingsSvc, logger)
	mappingKey := "search_mapping_version"
	if row, err := st.GetSetting(ctx, mappingKey); err == nil {
		var stored uint
		if err := json.Unmarshal(row.Value, &stored); err == nil && stored != search.MappingVersion() {
			go func() {
				if err := indexer.Rebuild(context.Background(), true, time.Time{}, nil); err != nil {
					logger.Error("rebuild search index", "error", err)
					return
				}
				raw, _ := json.Marshal(search.MappingVersion())
				_ = st.UpsertSetting(context.Background(), sqlcgen.UpsertSettingParams{Key: mappingKey, Value: raw})
				logger.Info("search index rebuilt", "mapping_version", search.MappingVersion())
			}()
		}
	} else {
		raw, _ := json.Marshal(search.MappingVersion())
		_ = st.UpsertSetting(ctx, sqlcgen.UpsertSettingParams{Key: mappingKey, Value: raw})
	}

	accounts.OnCreated = func(ctx context.Context, accountID uuid.UUID) { syncSvc.TriggerAccount(accountID) }
	accounts.OnScope = func(ctx context.Context, accountID uuid.UUID) { syncSvc.ReconcileScope(ctx, accountID) }
	accounts.OnDeleted = func(ctx context.Context, accountID uuid.UUID) {
		syncSvc.ForgetAccount(accountID)
		go purgeAccount(ctx, st, index, hub, settingsSvc, accountID, logger)
	}
	messages.AttachmentLimit = settingsSvc.MaxAttachmentBytes
	messages.OnArchive = func(ctx context.Context, entry sqlcgen.LocalArchive) error {
		return indexer.IndexArchive(ctx, entry)
	}

	authSvc, err := setupAuth(ctx, st, settingsSvc, cfg, logger)
	if err != nil {
		index.Close()
		st.Close()
		return nil, err
	}
	server := httpapi.NewServer(st, cfg, authSvc, accounts, ids, mailboxes, messages, composeSvc, settingsSvc, syncSvc, index, hub, blobs, logger, cipher)
	handler := httpapi.NewRouter(server, hub, logger)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	app := &App{
		cfg: cfg, logger: logger, store: st, blobs: blobs, index: index, hub: hub,
		settings: settingsSvc, sync: syncSvc, compose: composeSvc, messages: messages,
		accounts: accounts, server: httpServer, indexer: indexer,
	}
	return app, nil
}

func setupAuth(ctx context.Context, st *store.Store, settingsSvc *settings.Service, cfg *config.Config, logger *slog.Logger) (*auth.Service, error) {
	needs, err := st.CountUsers(ctx)
	if err != nil {
		return nil, err
	}
	token := cfg.SetupToken
	if needs == 0 && token == "" {
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		token = base64.RawURLEncoding.EncodeToString(buf)
		logger.Warn("setup token generated; open /setup and enter it", "setupToken", token)
	} else if needs == 0 && token != "" {
		logger.Warn("setup token configured; open /setup", "source", "MAILQUAY_SETUP_TOKEN")
	}
	return auth.NewService(st, settingsSvc, logger, token), nil
}

// Run starts background workers and serves HTTP until the context is cancelled.
func (a *App) Run(ctx context.Context) error {
	a.sync.Start(ctx)
	indexCtx, cancelIndex := context.WithCancel(ctx)
	defer cancelIndex()
	go a.indexer.Run(indexCtx)
	go a.compose.Run(ctx)
	go a.composeDraftPusher(ctx)
	go a.cleanupLoop(ctx)

	errCh := make(chan error, 1)
	go func() {
		a.logger.Info("http server listening", "addr", a.cfg.ListenAddr)
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		a.Close()
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = a.server.Shutdown(shutdownCtx)
	a.Close()
	return nil
}

func (a *App) composeDraftPusher(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.compose.PushDrafts(ctx)
		}
	}
}

func (a *App) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gcBlobs(ctx, a.store, a.blobs, a.logger)
			_ = a.store.CleanupTasks(ctx)
			_ = a.store.DeleteExpiredSessions(ctx)
			_ = a.store.DeleteExpiredOAuthStates(ctx)
			_ = a.store.ResetExpiredSearchJobs(ctx)
		}
	}
}

// Close releases process resources.
func (a *App) Close() {
	a.closeOnce.Do(func() {
		a.sync.Stop()
		a.sessions.Close()
		if a.index != nil {
			_ = a.index.Close()
		}
		if a.store != nil {
			a.store.Close()
		}
	})
}

// Reindex rebuilds the search index from PostgreSQL and blobs.
func (a *App) Reindex(ctx context.Context, all bool, since time.Time, mailboxID *uuid.UUID) error {
	return a.indexer.Rebuild(ctx, all, since, mailboxID)
}

func purgeAccount(ctx context.Context, st *store.Store, index *search.Index, hub *events.Hub,
	settingsSvc *settings.Service, accountID uuid.UUID, logger *slog.Logger) {
	taskID := uuid.MustParse(platform.NewID())
	now := time.Now().UTC()
	_, err := st.UpsertTask(ctx, sqlcgen.UpsertTaskParams{
		ID: taskID, Type: "account_purge", AccountID: &accountID, Status: "running",
		Unit: "messages", Message: "cleaning account", StartedAt: &now,
	})
	if err != nil {
		logger.Warn("start purge task", "account", accountID, "error", err)
	}
	preserve := settingsSvc.PreserveMarked(ctx)
	rows, err := st.ListMessagesForCleanup(ctx, accountID)
	if err != nil {
		logger.Warn("list messages for cleanup", "account", accountID, "error", err)
		return
	}
	done := int32(0)
	for _, msg := range rows {
		if preserve && msg.LocalFlag {
			_ = st.SetMessagePreservedAt(ctx, sqlcgen.SetMessagePreservedAtParams{ID: msg.ID, PreservedAt: &now})
			done++
			continue
		}
		if _, err := st.EnqueueSearchJob(ctx, sqlcgen.EnqueueSearchJobParams{
			ID: uuid.MustParse(platform.NewID()), MessageID: msg.ID, Op: "delete",
		}); err == nil {
			_ = index.Delete(ctx, []string{msg.ID.String()})
		}
		_ = st.DeleteMessage(ctx, msg.ID)
		done++
		if done%50 == 0 {
			_ = st.UpdateTaskProgress(ctx, sqlcgen.UpdateTaskProgressParams{ID: taskID, Done: done, Total: int32(len(rows)), Message: "cleaning account"})
		}
	}
	if preserve {
		// Folders stay for preserved messages; stop syncing and clear transient ops.
		_ = st.DeletePendingOpsByAccount(ctx, accountID)
	} else {
		_ = st.DeleteMailboxesByAccount(ctx, accountID)
		_ = st.HardDeleteAccount(ctx, accountID)
	}
	_ = st.CompleteTask(ctx, taskID)
	hub.Publish("task.updated", map[string]any{"taskId": taskID.String(), "type": "account_purge", "status": "completed"})
	hub.Publish("task.removed", map[string]any{"taskId": taskID.String()})
}

// Handler exposes the HTTP handler for integration tests and embedding.
func (a *App) Handler() http.Handler { return a.server.Handler }
