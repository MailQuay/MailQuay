package sync

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

func (s *Service) runScopeReconcile(ctx context.Context, acct sqlcgen.Account) {
	task, err := s.store.GetActiveTask(ctx, sqlcgen.GetActiveTaskParams{Type: "scope_reconcile", AccountID: &acct.ID})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Debug("load reconcile task", "account", acct.ID, "error", err)
		}
		return
	}
	rows, _ := s.store.ListMessageViewsForAccount(ctx, acct.ID)
	total := int32(len(rows))
	done := int32(0)
	if acct.InitialSyncCutoffAt != nil {
		cutoff := acct.InitialSyncCutoffAt.UTC()
		old, err := s.store.ListMessagesOlderThan(ctx, sqlcgen.ListMessagesOlderThanParams{AccountID: acct.ID, ReceivedAt: cutoff})
		if err == nil {
			for _, msg := range old {
				if msg.LocalFlag || msg.PreservedAt != nil {
					continue
				}
				s.enqueueSearch(ctx, msg.ID, "delete")
				if err := s.store.DeleteMessage(ctx, msg.ID); err != nil {
					s.logger.Debug("drop out-of-range message", "message", msg.ID, "error", err)
				}
				done++
			}
		}
	}
	switch acct.DownloadScope {
	case "headers":
		if err := s.store.ClearMessageBodyCache(ctx, sqlcgen.ClearMessageBodyCacheParams{AccountID: acct.ID}); err != nil {
			s.logger.Debug("clear body cache", "account", acct.ID, "error", err)
		}
		if err := s.store.DeleteAttachmentsForAccount(ctx, sqlcgen.DeleteAttachmentsForAccountParams{
			AccountID: acct.ID, KeepInline: false,
		}); err != nil {
			s.logger.Debug("clear attachments", "account", acct.ID, "error", err)
		}
	case "full_no_attachments":
		if err := s.store.ClearMessageAttachmentCache(ctx, sqlcgen.ClearMessageAttachmentCacheParams{AccountID: acct.ID}); err != nil {
			s.logger.Debug("clear attachment cache", "account", acct.ID, "error", err)
		}
		if err := s.store.DeleteAttachmentsForAccount(ctx, sqlcgen.DeleteAttachmentsForAccountParams{
			AccountID: acct.ID, KeepInline: true,
		}); err != nil {
			s.logger.Debug("clear attachments", "account", acct.ID, "error", err)
		}
	}
	if total == 0 {
		total = done
	}
	_ = s.store.UpdateTaskProgress(ctx, sqlcgen.UpdateTaskProgressParams{
		ID: task.ID, Done: total, Total: total, Message: "scope reconciled",
	})
	_ = s.store.CompleteTask(ctx, task.ID)
	s.hub.Publish("task.updated", map[string]any{
		"taskId": task.ID.String(), "type": "scope_reconcile", "status": "completed", "done": total, "total": total,
	})
	s.hub.Publish("task.removed", map[string]any{"taskId": task.ID.String()})
	_ = done
	_ = time.Now
}
