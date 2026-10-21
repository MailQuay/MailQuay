package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/imapclient"
	"github.com/mailquay/mailquay/internal/mailsession"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// opsWait bounds how long the background ops loop waits for the account
// session before it retries on the next tick.
const opsWait = 2 * time.Second

func (s *Service) opsWorker(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.processPendingOps(ctx)
		}
	}
}

func (s *Service) processPendingOps(ctx context.Context) {
	accounts, err := s.store.ListActiveAccounts(ctx)
	if err != nil {
		return
	}
	for _, acct := range accounts {
		switch acct.ReceiveProtocol {
		case "imap":
			if err := s.processAccountOps(ctx, acct); err != nil && !errors.Is(err, mailsession.ErrBusy) {
				s.logger.Debug("process pending ops", "account", acct.ID, "error", err)
			}
		case "pop3":
			if err := s.processPOP3AccountOps(ctx, acct); err != nil {
				s.logger.Debug("process pop3 ops", "account", acct.ID, "error", err)
			}
		}
	}
}

func (s *Service) processAccountOps(ctx context.Context, acct sqlcgen.Account) error {
	ops, err := s.store.ListPendingOps(ctx, sqlcgen.ListPendingOpsParams{AccountID: acct.ID, Limit: 50})
	if err != nil || len(ops) == 0 {
		return err
	}
	creds, err := s.accounts.Credentials(ctx, acct)
	if err != nil {
		return err
	}
	return s.sessions.TryDo(ctx, acct.ID, opsWait, s.imapDialer(acct, creds), func(ctx context.Context, client *imapclient.Client) error {
		return s.applyAccountOps(ctx, client, acct, ops)
	})
}

func (s *Service) applyAccountOps(ctx context.Context, client *imapclient.Client, acct sqlcgen.Account, ops []sqlcgen.PendingOp) error {
	selected := ""
	for _, op := range ops {
		if op.MailboxID == nil {
			continue
		}
		mb, err := s.store.GetMailbox(ctx, *op.MailboxID)
		if err != nil {
			_ = s.store.CompletePendingOp(ctx, op.ID)
			continue
		}
		if mb.SyncMode == "off" || mb.LocalOnly || mb.Role == "sent" {
			_ = s.store.CompletePendingOp(ctx, op.ID)
			continue
		}
		if selected != mb.Path {
			if _, err := client.Select(ctx, mb.Path); err != nil {
				s.retryOp(ctx, op, err)
				continue
			}
			selected = mb.Path
		}
		var execErr error
		switch op.Kind {
		case "flags":
			var payload struct {
				Add   bool     `json:"add"`
				Flags []string `json:"flags"`
			}
			_ = json.Unmarshal(op.Payload, &payload)
			if op.Uid == nil {
				execErr = errors.New("missing uid")
				break
			}
			execErr = client.StoreFlags(ctx, uint32(*op.Uid), payload.Flags, payload.Add)
		case "move":
			var payload struct {
				Dest string `json:"dest"`
			}
			_ = json.Unmarshal(op.Payload, &payload)
			if op.Uid == nil || payload.Dest == "" {
				execErr = errors.New("invalid move payload")
				break
			}
			if err := client.EnsureMailbox(ctx, payload.Dest); err != nil {
				execErr = err
				break
			}
			execErr = client.Move(ctx, uint32(*op.Uid), payload.Dest)
		case "delete":
			if op.Uid == nil {
				execErr = errors.New("missing uid")
				break
			}
			if err := client.StoreFlags(ctx, uint32(*op.Uid), []string{"\\Deleted"}, true); err != nil {
				execErr = err
				break
			}
			execErr = client.Expunge(ctx, []uint32{uint32(*op.Uid)})
		default:
			execErr = fmt.Errorf("unsupported op %q", op.Kind)
		}
		if execErr != nil {
			s.retryOp(ctx, op, execErr)
			continue
		}
		if err := s.store.CompletePendingOp(ctx, op.ID); err != nil {
			s.logger.Warn("complete pending op", "op", op.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) retryOp(ctx context.Context, op sqlcgen.PendingOp, cause error) {
	attempts := int(op.Attempts) + 1
	if attempts >= 10 {
		_ = s.store.FailPendingOp(ctx, sqlcgen.FailPendingOpParams{
			ID: op.ID, Status: "failed", LastError: cause.Error(), NextAttemptAt: time.Now().UTC().Add(24 * time.Hour),
		})
		return
	}
	delay := platform.Backoff(attempts, 30*time.Second, 30*time.Minute)
	_ = s.store.FailPendingOp(ctx, sqlcgen.FailPendingOpParams{
		ID: op.ID, Status: "pending", LastError: cause.Error(), NextAttemptAt: time.Now().UTC().Add(delay),
	})
}

func (s *Service) deleteQueueWorker(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.processDeleteQueue(ctx)
		}
	}
}

func (s *Service) processDeleteQueue(ctx context.Context) {
	items, err := s.store.ListDueDeletes(ctx, 100)
	if err != nil || len(items) == 0 {
		return
	}
	for _, item := range items {
		msgErr := s.deleteQueued(ctx, item)
		if msgErr != nil {
			_ = s.store.MarkDeleteFailed(ctx, sqlcgen.MarkDeleteFailedParams{ID: item.ID, LastError: msgErr.Error()})
			continue
		}
		_ = s.store.MarkDeleteDone(ctx, item.ID)
	}
}

func (s *Service) deleteQueued(ctx context.Context, item sqlcgen.DeleteQueue) error {
	msg, err := s.store.GetMessage(ctx, item.MessageID)
	if err != nil {
		return nil
	}
	acct, err := s.store.GetAccount(ctx, item.AccountID)
	if err != nil {
		return err
	}
	if acct.ReceiveProtocol == "imap" && msg.Uid != nil {
		payload, _ := json.Marshal(map[string]any{})
		if _, err := s.store.EnqueuePendingOp(ctx, sqlcgen.EnqueuePendingOpParams{
			ID: uuid.MustParse(platform.NewID()), AccountID: msg.AccountID,
			MessageID: &msg.ID, MailboxID: &msg.MailboxID, Uid: msg.Uid, Kind: "delete", Payload: payload,
		}); err != nil {
			return err
		}
	}
	return s.messages.DeleteLocal(ctx, msg)
}

func (s *Service) startIdleWorker(parent context.Context, accountID uuid.UUID) {
	s.mu.Lock()
	if _, exists := s.idle[accountID]; exists {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.idle[accountID] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.idle, accountID)
			s.mu.Unlock()
			cancel()
		}()
		s.idleLoop(ctx, accountID)
	}()
}

func (s *Service) idleLoop(ctx context.Context, accountID uuid.UUID) {
	acct, err := s.store.GetAccount(ctx, accountID)
	if err != nil || acct.DeletedAt != nil || !acct.SyncEnabled || acct.ReceiveProtocol != "imap" {
		return
	}
	creds, err := s.accounts.Credentials(ctx, acct)
	if err != nil {
		return
	}
	backoff := 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		client, err := s.dialIMAP(ctx, s.imapConfig(acct, creds))
		if err != nil {
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < 10*time.Minute {
				backoff *= 2
			}
			continue
		}
		inbox, err := s.store.GetMailboxByRole(ctx, sqlcgen.GetMailboxByRoleParams{AccountID: acct.ID, Role: "inbox"})
		if err == nil {
			if _, err := client.Select(ctx, inbox.Path); err == nil {
				for ctx.Err() == nil {
					changed, err := client.Idle(ctx, 25*time.Minute)
					if err != nil {
						break
					}
					if changed {
						s.TriggerAccount(acct.ID)
					}
				}
			}
		}
		_ = client.Close()
		if !sleepCtx(ctx, 5*time.Second) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Service) processPOP3AccountOps(ctx context.Context, acct sqlcgen.Account) error {
	ops, err := s.store.ListPendingOps(ctx, sqlcgen.ListPendingOpsParams{AccountID: acct.ID, Limit: 50})
	if err != nil || len(ops) == 0 {
		return err
	}
	deletions := make([]sqlcgen.PendingOp, 0, len(ops))
	for _, op := range ops {
		if op.Kind == "pop3_delete" {
			deletions = append(deletions, op)
		} else {
			_ = s.store.CompletePendingOp(ctx, op.ID)
		}
	}
	if len(deletions) == 0 {
		return nil
	}
	creds, err := s.accounts.Credentials(ctx, acct)
	if err != nil {
		return err
	}
	client, err := s.dialPOP3(ctx, s.pop3Config(acct, creds))
	if err != nil {
		return err
	}
	defer client.Close()
	infos, err := client.UIDL(ctx)
	if err != nil {
		return err
	}
	byUIDL := make(map[string]int, len(infos))
	for _, info := range infos {
		byUIDL[info.UIDL] = info.Number
	}
	comitted := false
	for _, op := range deletions {
		var payload struct {
			UIDL string `json:"uidl"`
		}
		_ = json.Unmarshal(op.Payload, &payload)
		number, ok := byUIDL[payload.UIDL]
		if !ok {
			_ = s.store.CompletePendingOp(ctx, op.ID)
			continue
		}
		if err := client.Delete(ctx, number); err != nil {
			s.retryOp(ctx, op, err)
			continue
		}
		comitted = true
	}
	if comitted {
		if err := client.Quit(ctx); err != nil {
			return err
		}
	}
	for _, op := range deletions {
		_ = s.store.CompletePendingOp(ctx, op.ID)
	}
	return nil
}
