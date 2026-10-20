package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mailquay/mailquay/internal/events"
	"github.com/mailquay/mailquay/internal/mime"
	"github.com/mailquay/mailquay/internal/platform"
	"github.com/mailquay/mailquay/internal/search"
	"github.com/mailquay/mailquay/internal/storage"
	"github.com/mailquay/mailquay/internal/store"
	"github.com/mailquay/mailquay/internal/store/sqlcgen"
)

// Indexer drains search_jobs into the Bleve index.
type Indexer struct {
	store  *store.Store
	index  *search.Index
	blobs  *storage.Store
	hub    *events.Hub
	log    *slog.Logger
	mu     sync.Mutex
	taskID uuid.UUID
	total  int32
	done   int32
}

// NewIndexer builds the index worker.
func NewIndexer(st *store.Store, index *search.Index, blobs *storage.Store, hub *events.Hub, logger *slog.Logger) *Indexer {
	return &Indexer{store: st, index: index, blobs: blobs, hub: hub, log: logger}
}

// Run processes index jobs until the context is cancelled.
func (i *Indexer) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for n := 0; n < 5; n++ {
				count, err := i.processBatch(ctx)
				if err != nil {
					i.log.Warn("index batch", "error", err)
					break
				}
				if count == 0 {
					break
				}
			}
		}
	}
}

func (i *Indexer) processBatch(ctx context.Context) (int, error) {
	if err := i.store.ResetExpiredSearchJobs(ctx); err != nil {
		i.log.Debug("reset search jobs", "error", err)
	}
	lease := time.Now().UTC().Add(2 * time.Minute)
	jobs, err := i.store.ClaimSearchJobs(ctx, sqlcgen.ClaimSearchJobsParams{LeaseUntil: &lease, Limit: 100})
	if err != nil {
		return 0, err
	}
	if len(jobs) == 0 {
		i.completeTask(ctx)
		return 0, nil
	}
	i.startTask(ctx, len(jobs))
	var docs []search.Document
	var deletes []string
	var doneIDs []uuid.UUID
	for _, job := range jobs {
		switch job.Op {
		case "delete":
			deletes = append(deletes, job.MessageID.String())
			doneIDs = append(doneIDs, job.ID)
		case "index":
			msg, err := i.store.GetMessage(ctx, job.MessageID)
			if err == nil {
				doc, err := i.documentForMessage(ctx, msg)
				if err != nil {
					i.retryJob(ctx, job, err)
					continue
				}
				docs = append(docs, doc)
				doneIDs = append(doneIDs, job.ID)
				continue
			}
			archive, archErr := i.store.GetArchive(ctx, job.MessageID)
			if archErr != nil {
				doneIDs = append(doneIDs, job.ID)
				continue
			}
			doc, err := i.documentForArchive(ctx, archive)
			if err != nil {
				i.retryJob(ctx, job, err)
				continue
			}
			docs = append(docs, doc)
			doneIDs = append(doneIDs, job.ID)
		default:
			doneIDs = append(doneIDs, job.ID)
		}
	}
	if len(deletes) > 0 {
		if err := i.index.Delete(ctx, deletes); err != nil {
			for _, job := range jobs {
				if job.Op == "delete" {
					i.retryJob(ctx, job, err)
				}
			}
			return 0, err
		}
	}
	if len(docs) > 0 {
		if err := i.index.Upsert(ctx, docs); err != nil {
			for _, job := range jobs {
				if job.Op == "index" {
					i.retryJob(ctx, job, err)
				}
			}
			return 0, err
		}
	}
	for _, id := range doneIDs {
		_ = i.store.CompleteSearchJob(ctx, id)
	}
	i.advanceTask(ctx, len(doneIDs))
	return len(jobs), nil
}

func (i *Indexer) startTask(ctx context.Context, batch int) {
	i.mu.Lock()
	needNew := i.taskID == uuid.Nil
	i.mu.Unlock()
	pending, _ := i.store.CountPendingSearchJobs(ctx)
	total := int32(pending)
	if total < int32(batch) {
		total = int32(batch)
	}
	if needNew {
		now := time.Now().UTC()
		task, err := i.store.UpsertTask(ctx, sqlcgen.UpsertTaskParams{
			ID: uuid.MustParse(platform.NewID()), Type: "search_index", Status: "running",
			Total: total, Unit: "documents", Message: "indexing mail", StartedAt: &now,
		})
		if err != nil {
			return
		}
		i.mu.Lock()
		i.taskID = task.ID
		i.total = total
		i.done = 0
		i.mu.Unlock()
		i.publishTask(ctx, task)
	}
}

func (i *Indexer) advanceTask(ctx context.Context, delta int) {
	i.mu.Lock()
	id := i.taskID
	i.done += int32(delta)
	done := i.done
	total := i.total
	i.mu.Unlock()
	if id == uuid.Nil {
		return
	}
	if done > total {
		total = done
	}
	_ = i.store.UpdateTaskProgress(ctx, sqlcgen.UpdateTaskProgressParams{ID: id, Done: done, Total: total, Message: "indexing mail"})
	i.publishTaskID(ctx, id)
}

func (i *Indexer) completeTask(ctx context.Context) {
	i.mu.Lock()
	id := i.taskID
	i.taskID = uuid.Nil
	i.done = 0
	i.total = 0
	i.mu.Unlock()
	if id == uuid.Nil {
		return
	}
	_ = i.store.CompleteTask(ctx, id)
	i.publishTaskID(ctx, id)
	i.hub.Publish("task.removed", map[string]any{"taskId": id.String()})
}

func (i *Indexer) publishTask(ctx context.Context, task sqlcgen.Task) {
	i.hub.Publish("task.updated", map[string]any{
		"taskId": task.ID.String(), "type": "search_index", "status": task.Status,
		"done": task.Done, "total": task.Total, "unit": "documents", "message": task.Message,
	})
}

func (i *Indexer) publishTaskID(ctx context.Context, id uuid.UUID) {
	task, err := i.store.GetTask(ctx, id)
	if err != nil {
		return
	}
	i.publishTask(ctx, task)
}

func (i *Indexer) retryJob(ctx context.Context, job sqlcgen.SearchJob, cause error) {
	attempts := int(job.Attempts) + 1
	if attempts >= 10 {
		_ = i.store.GiveUpSearchJob(ctx, sqlcgen.GiveUpSearchJobParams{ID: job.ID, LastError: cause.Error()})
		i.log.Error("search job failed permanently", "message", job.MessageID, "error", cause)
		return
	}
	next := time.Now().UTC().Add(platform.Backoff(attempts, 10*time.Second, 10*time.Minute))
	_ = i.store.FailSearchJob(ctx, sqlcgen.FailSearchJobParams{ID: job.ID, LastError: cause.Error(), NextAttemptAt: next})
}

func (i *Indexer) documentForMessage(ctx context.Context, msg sqlcgen.Message) (search.Document, error) {
	body := ""
	if msg.RawSha256 != "" {
		if text, err := i.blobs.ReadText(msg.RawSha256); err == nil {
			body = string(text)
		} else if raw, err := i.readBlob(msg.RawSha256); err == nil {
			parsed, err := mime.Parse(raw, mime.Options{MaxAttachmentBytes: 25 << 20})
			if err == nil {
				body = parsed.Text
			}
		}
	}
	var from searchAddress
	if len(msg.FromAddr) > 0 {
		_ = json.Unmarshal(msg.FromAddr, &from)
	}
	to := decodeSearchAddresses(msg.ToAddrs)
	cc := decodeSearchAddresses(msg.CcAddrs)
	bcc := decodeSearchAddresses(msg.BccAddrs)
	sentAt := time.Time{}
	if msg.SentAt != nil {
		sentAt = msg.SentAt.UTC()
	}
	return search.Document{
		ID: msg.ID.String(), AccountID: msg.AccountID.String(), MailboxID: msg.MailboxID.String(),
		Subject: msg.Subject, Body: body,
		From: []string{from.display()}, To: displayList(to), Cc: displayList(cc), Bcc: displayList(bcc),
		FromAddress: from.Email, ToAddress: addressList(to), CcAddress: addressList(cc), BccAddress: addressList(bcc),
		ReceivedAt: msg.ReceivedAt.UTC(), SentAt: sentAt,
		Unread: !hasFlag(msg.Flags, "\\Seen"), Marked: msg.LocalFlag,
		HasAttachments: msg.HasAttachments, Size: msg.Size, Flags: msg.Flags,
	}, nil
}

func (i *Indexer) documentForArchive(ctx context.Context, row sqlcgen.LocalArchive) (search.Document, error) {
	body := ""
	if row.TextSha256 != "" {
		if text, err := i.blobs.ReadText(row.TextSha256); err == nil {
			body = string(text)
		}
	}
	if body == "" && row.RawSha256 != "" {
		if raw, err := i.readBlob(row.RawSha256); err == nil {
			if parsed, err := mime.Parse(raw, mime.Options{MaxAttachmentBytes: 25 << 20}); err == nil {
				body = parsed.Text
			}
		}
	}
	var from searchAddress
	if len(row.FromAddr) > 0 {
		_ = json.Unmarshal(row.FromAddr, &from)
	}
	to := decodeSearchAddresses(row.ToAddrs)
	cc := decodeSearchAddresses(row.CcAddrs)
	bcc := decodeSearchAddresses(row.BccAddrs)
	sentAt := time.Time{}
	if row.SentAt != nil {
		sentAt = row.SentAt.UTC()
	}
	return search.Document{
		ID: row.ID.String(), AccountID: derefUUID(row.OriginAccountID), MailboxID: "",
		Subject: row.Subject, Body: body,
		From: []string{from.display()}, To: displayList(to), Cc: displayList(cc), Bcc: displayList(bcc),
		FromAddress: from.Email, ToAddress: addressList(to), CcAddress: addressList(cc), BccAddress: addressList(bcc),
		ReceivedAt: row.ReceivedAt.UTC(), SentAt: sentAt, HasAttachments: row.HasAttachments,
		Size: row.Size, IsArchive: true,
	}, nil
}

// IndexArchive indexes a freshly created archive entry.
func (i *Indexer) IndexArchive(ctx context.Context, row sqlcgen.LocalArchive) error {
	doc, err := i.documentForArchive(ctx, row)
	if err != nil {
		return err
	}
	return i.index.Upsert(ctx, []search.Document{doc})
}

// Rebuild clears and repopulates the index.
func (i *Indexer) Rebuild(ctx context.Context, all bool, since time.Time, mailboxID *uuid.UUID) error {
	if err := i.index.Reset(ctx); err != nil {
		return err
	}
	rows, err := i.store.ListAllMessages(ctx)
	if err != nil {
		return err
	}
	batch := make([]search.Document, 0, 200)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := i.index.Upsert(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for _, msg := range rows {
		if mailboxID != nil && msg.MailboxID != *mailboxID {
			continue
		}
		if !since.IsZero() && msg.ReceivedAt.Before(since) {
			continue
		}
		doc, err := i.documentForMessage(ctx, msg)
		if err != nil {
			continue
		}
		batch = append(batch, doc)
		if len(batch) >= 200 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	archives, err := i.store.ListAllArchivesForIndex(ctx)
	if err != nil {
		return err
	}
	for _, row := range archives {
		doc, err := i.documentForArchive(ctx, row)
		if err != nil {
			continue
		}
		batch = append(batch, doc)
		if len(batch) >= 200 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (i *Indexer) readBlob(sha string) ([]byte, error) {
	reader, err := i.blobs.OpenBlob(sha)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return ioReadAll(reader)
}

type searchAddress struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (a searchAddress) display() string {
	if a.Name == "" {
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

func decodeSearchAddresses(raw json.RawMessage) []searchAddress {
	var out []searchAddress
	_ = json.Unmarshal(raw, &out)
	return out
}

func displayList(addrs []searchAddress) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.display())
	}
	return out
}

func addressList(addrs []searchAddress) string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.Email)
	}
	return strings.Join(out, " ")
}

func derefUUID(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func ioReadAll(reader io.Reader) ([]byte, error) { return io.ReadAll(reader) }
