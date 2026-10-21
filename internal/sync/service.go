package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/account"
	"github.com/mailquay/mailquay/internal/config"
	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/mailbox"
	"github.com/mailquay/mailquay/internal/message"
	"github.com/mailquay/mailquay/internal/netpace"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/pop3client"
	"github.com/mailquay/mailquay/internal/settings"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// Service runs account synchronization, content fetching and remote operations.
type Service struct {
	store     *store.Store
	cfg       *config.Config
	logger    *slog.Logger
	hub       *events.Hub
	storage   *storage.Store
	accounts  *account.Service
	mailboxes *mailbox.Service
	messages  *message.Service
	settings  *settings.Service
	pacer     *netpace.Pacer

	fetchLim *limiter
	syncLim  *limiter

	mu       sync.Mutex
	lastRun  map[uuid.UUID]time.Time
	failures map[uuid.UUID]int
	workers  map[uuid.UUID]context.CancelFunc
	idle     map[uuid.UUID]context.CancelFunc
	taskIDs  map[string]uuid.UUID
	trigger  chan uuid.UUID
	bodyCh   chan bodyRequest
	stopOnce sync.Once
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

type bodyRequest struct {
	messageID uuid.UUID
	force     bool
}

// NewService wires the synchronization service.
func NewService(st *store.Store, cfg *config.Config, logger *slog.Logger, hub *events.Hub,
	blobs *storage.Store, accounts *account.Service, mailboxes *mailbox.Service,
	messages *message.Service, settingsSvc *settings.Service, pacer *netpace.Pacer) *Service {
	concurrency := settingsSvc.Resources()
	syncN := 2
	fetchN := 4
	if snap, err := settingsSvc.Get(context.Background()); err == nil {
		syncN = snap.Effective.SyncConcurrency
		fetchN = snap.Effective.FetchConcurrency
	}
	_ = concurrency
	return &Service{
		store: st, cfg: cfg, logger: logger, hub: hub, storage: blobs,
		accounts: accounts, mailboxes: mailboxes, messages: messages, settings: settingsSvc,
		pacer:    pacer,
		fetchLim: newLimiter(fetchN), syncLim: newLimiter(syncN),
		workers: make(map[uuid.UUID]context.CancelFunc), lastRun: make(map[uuid.UUID]time.Time),
		failures: make(map[uuid.UUID]int), idle: make(map[uuid.UUID]context.CancelFunc),
		taskIDs: make(map[string]uuid.UUID), trigger: make(chan uuid.UUID, 64),
		bodyCh: make(chan bodyRequest, 512),
	}
}

// Start launches the scheduler, operation workers and background loops.
func (s *Service) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.messages.SetContent(s)
	s.wg.Add(1)
	go s.scheduler(ctx)
	s.wg.Add(1)
	go s.bodyWorker(ctx)
	s.wg.Add(1)
	go s.opsWorker(ctx)
	s.wg.Add(1)
	go s.deleteQueueWorker(ctx)
}

// Stop terminates all workers.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Lock()
		for _, cancel := range s.workers {
			cancel()
		}
		for _, cancel := range s.idle {
			cancel()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
}

// SyncNow synchronizes one account synchronously (used by integration tests and manual triggers).
func (s *Service) SyncNow(ctx context.Context, accountID uuid.UUID) error {
	return s.syncAccount(ctx, accountID)
}

// TriggerAccount requests an immediate sync.
func (s *Service) TriggerAccount(id uuid.UUID) {
	select {
	case s.trigger <- id:
	default:
	}
}

// TriggerMailbox requests a sync of the account owning the mailbox.
func (s *Service) TriggerMailbox(ctx context.Context, mailboxID uuid.UUID) error {
	mb, err := s.store.GetMailbox(ctx, mailboxID)
	if err != nil {
		return err
	}
	s.TriggerAccount(mb.AccountID)
	return nil
}

// ReconcileScope applies range and download-scope changes after an account edit.
func (s *Service) ReconcileScope(ctx context.Context, accountID uuid.UUID) {
	if err := s.startTask(ctx, "scope_reconcile", &accountID, 0, 0, "reconciling scope"); err != nil {
		s.logger.Warn("start reconcile task", "account", accountID, "error", err)
	}
	s.TriggerAccount(accountID)
}

func (s *Service) scheduler(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	s.triggerDue(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.trigger:
			s.startAccountWorker(ctx, id)
		case <-ticker.C:
			s.triggerDue(ctx, false)
		}
	}
}

func (s *Service) triggerDue(ctx context.Context, force bool) {
	accounts, err := s.store.ListActiveAccounts(ctx)
	if err != nil {
		s.logger.Warn("list active accounts", "error", err)
		return
	}
	values := s.settings.Current()
	now := time.Now()
	for _, acct := range accounts {
		interval := time.Duration(values.SyncIntervalSeconds) * time.Second
		if acct.ReceiveProtocol == "pop3" {
			interval = time.Duration(values.POP3IntervalSeconds) * time.Second
		}
		s.mu.Lock()
		last, seen := s.lastRun[acct.ID]
		failures := s.failures[acct.ID]
		due := force || !seen || now.Sub(last) >= backoffDelay(interval, failures)
		if due {
			s.lastRun[acct.ID] = now
		}
		s.mu.Unlock()
		if due {
			s.TriggerAccount(acct.ID)
		}
		if acct.ReceiveProtocol == "imap" {
			s.startIdleWorker(ctx, acct.ID)
		}
	}
}

func (s *Service) startAccountWorker(parent context.Context, accountID uuid.UUID) {
	s.mu.Lock()
	if _, running := s.workers[accountID]; running {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.workers[accountID] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.workers, accountID)
			s.mu.Unlock()
			cancel()
		}()
		if snap, err := s.settings.Get(ctx); err == nil {
			s.syncLim.setLimit(snap.Effective.SyncConcurrency)
			s.fetchLim.setLimit(snap.Effective.FetchConcurrency)
		}
		if !s.syncLim.acquire(ctx) {
			return
		}
		defer s.syncLim.release()
		if err := s.syncAccount(ctx, accountID); err != nil {
			s.logger.Warn("sync account", "account", accountID, "error", err)
			s.recordSyncFailure(accountID)
			return
		}
		s.recordSyncSuccess(accountID)
	}()
}

// backoffDelay stretches the scheduled sync interval after consecutive
// failures so an unreachable or throttled provider is not reconnected on every
// tick. The multiplier is capped at 8x the configured interval.
func backoffDelay(interval time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return interval
	}
	if failures > 3 {
		failures = 3
	}
	return interval * time.Duration(1<<failures)
}

func (s *Service) recordSyncFailure(accountID uuid.UUID) {
	s.mu.Lock()
	if s.failures[accountID] < 8 {
		s.failures[accountID]++
	}
	s.mu.Unlock()
}

func (s *Service) recordSyncSuccess(accountID uuid.UUID) {
	s.mu.Lock()
	delete(s.failures, accountID)
	s.mu.Unlock()
}

func (s *Service) syncAccount(ctx context.Context, accountID uuid.UUID) error {
	acct, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load account: %w", err)
	}
	if acct.DeletedAt != nil || !acct.SyncEnabled {
		return nil
	}
	creds, err := s.accounts.Credentials(ctx, acct)
	if err != nil {
		s.setAccountError(ctx, acct.ID, err)
		return err
	}
	s.runScopeReconcile(ctx, acct)
	var syncErr error
	switch acct.ReceiveProtocol {
	case "imap":
		syncErr = s.syncIMAP(ctx, acct, creds)
	case "pop3":
		syncErr = s.syncPOP3(ctx, acct, creds)
	default:
		syncErr = fmt.Errorf("unsupported protocol %q", acct.ReceiveProtocol)
	}
	if syncErr != nil {
		s.setAccountError(ctx, acct.ID, syncErr)
		s.failTaskForAccount(ctx, "initial_sync", acct.ID, syncErr)
		s.failTaskForAccount(ctx, "mailbox_sync", acct.ID, syncErr)
		return syncErr
	}
	_ = s.store.SetAccountState(ctx, sqlcgen.SetAccountStateParams{ID: acct.ID, SyncState: "connected", LastError: ""})
	return nil
}

// dialIMAP reserves this host's next connection slot before dialing. Providers
// throttle by connection rate, so every outbound dial in this service is paced
// regardless of local concurrency.
func (s *Service) dialIMAP(ctx context.Context, cfg imapclient.Config) (*imapclient.Client, error) {
	if !netpace.IsInteractive(ctx) {
		if err := s.pacer.Wait(ctx, cfg.Address); err != nil {
			return nil, err
		}
	}
	return imapclient.Dial(ctx, cfg)
}

// dialPOP3 is the POP3 counterpart of dialIMAP.
func (s *Service) dialPOP3(ctx context.Context, cfg pop3client.Config) (*pop3client.Client, error) {
	if !netpace.IsInteractive(ctx) {
		if err := s.pacer.Wait(ctx, cfg.Address); err != nil {
			return nil, err
		}
	}
	return pop3client.Dial(ctx, cfg)
}

func (s *Service) setAccountError(ctx context.Context, accountID uuid.UUID, cause error) {
	_ = s.store.SetAccountState(ctx, sqlcgen.SetAccountStateParams{ID: accountID, SyncState: "error", LastError: cause.Error()})
	s.hub.Publish("account.updated", map[string]any{"accountId": accountID.String(), "state": "error", "error": cause.Error()})
}

func (s *Service) taskKey(typ string, accountID *uuid.UUID) string {
	if accountID == nil {
		return typ
	}
	return typ + ":" + accountID.String()
}

func (s *Service) startTask(ctx context.Context, typ string, accountID *uuid.UUID, done, total int32, msg string) error {
	now := time.Now().UTC()
	task, err := s.store.RestartFailedTask(ctx, sqlcgen.RestartFailedTaskParams{
		Done: done, Total: total, Message: msg, StartedAt: &now, Type: typ, AccountID: accountID,
	})
	if err != nil {
		task, err = s.store.UpsertTask(ctx, sqlcgen.UpsertTaskParams{
			ID: uuid.MustParse(platform.NewID()), Type: typ, AccountID: accountID,
			Status: "running", Done: done, Total: total, Unit: "messages", Message: msg, StartedAt: &now,
		})
	}
	if err != nil {
		return fmt.Errorf("upsert task: %w", err)
	}
	s.mu.Lock()
	s.taskIDs[s.taskKey(typ, accountID)] = task.ID
	s.mu.Unlock()
	s.publishTask(task)
	return nil
}

func (s *Service) updateTask(ctx context.Context, typ string, accountID *uuid.UUID, done, total int32, msg string) {
	s.mu.Lock()
	id, ok := s.taskIDs[s.taskKey(typ, accountID)]
	s.mu.Unlock()
	if !ok {
		if err := s.startTask(ctx, typ, accountID, done, total, msg); err != nil {
			s.logger.Debug("start task", "type", typ, "error", err)
		}
		return
	}
	if err := s.store.UpdateTaskProgress(ctx, sqlcgen.UpdateTaskProgressParams{ID: id, Done: done, Total: total, Message: msg}); err != nil {
		s.logger.Debug("update task", "type", typ, "error", err)
		return
	}
	s.publishTaskID(ctx, id)
}

func (s *Service) completeTask(ctx context.Context, typ string, accountID *uuid.UUID) {
	s.mu.Lock()
	id, ok := s.taskIDs[s.taskKey(typ, accountID)]
	delete(s.taskIDs, s.taskKey(typ, accountID))
	s.mu.Unlock()
	if !ok {
		return
	}
	if err := s.store.CompleteTask(ctx, id); err != nil {
		s.logger.Debug("complete task", "error", err)
		return
	}
	s.publishTaskID(ctx, id)
	s.hub.Publish("task.removed", map[string]any{"taskId": id.String()})
}

func (s *Service) failTaskForAccount(ctx context.Context, typ string, accountID uuid.UUID, cause error) {
	s.mu.Lock()
	id, ok := s.taskIDs[s.taskKey(typ, &accountID)]
	delete(s.taskIDs, s.taskKey(typ, &accountID))
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = s.store.FailTask(ctx, sqlcgen.FailTaskParams{ID: id, Error: cause.Error()})
	s.publishTaskID(ctx, id)
}

func (s *Service) publishTask(task sqlcgen.Task) {
	s.hub.Publish("task.updated", taskPayload(task))
}

func (s *Service) publishTaskID(ctx context.Context, id uuid.UUID) {
	task, err := s.store.GetTask(ctx, id)
	if err != nil {
		return
	}
	s.publishTask(task)
}

func taskPayload(task sqlcgen.Task) map[string]any {
	payload := map[string]any{
		"taskId": task.ID.String(), "type": task.Type, "status": task.Status,
		"done": task.Done, "total": task.Total, "unit": task.Unit, "message": task.Message,
	}
	if task.AccountID != nil {
		payload["accountId"] = task.AccountID.String()
	}
	if task.Error != "" {
		payload["error"] = task.Error
	}
	return payload
}

func (s *Service) enqueueBody(messageID uuid.UUID, force bool) {
	select {
	case s.bodyCh <- bodyRequest{messageID: messageID, force: force}:
	default:
	}
}

func (s *Service) bodyWorker(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-s.bodyCh:
			if snap, err := s.settings.Get(ctx); err == nil {
				s.fetchLim.setLimit(snap.Effective.FetchConcurrency)
			}
			if !s.fetchLim.acquire(ctx) {
				return
			}
			go func(req bodyRequest) {
				defer s.fetchLim.release()
				fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				if _, err := s.messages.Body(fctx, req.messageID, req.force); err != nil {
					s.logger.Debug("prefetch body", "message", req.messageID, "error", err)
				}
			}(req)
		}
	}
}

func (s *Service) enqueueSearch(ctx context.Context, messageID uuid.UUID, op string) {
	if _, err := s.store.EnqueueSearchJob(ctx, sqlcgen.EnqueueSearchJobParams{
		ID: uuid.MustParse(platform.NewID()), MessageID: messageID, Op: op,
	}); err != nil {
		s.logger.Warn("enqueue search job", "message", messageID, "error", err)
	}
}

func encodePayload(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
