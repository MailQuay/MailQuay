package search

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/index/scorch"
)

// Sentinel errors returned by the index lifecycle.
var (
	// ErrLocked means another handle, in this process or another one,
	// already has the index open.
	ErrLocked = errors.New("search index is locked")
	// ErrUnavailable means the index could not be opened or has been closed.
	ErrUnavailable = errors.New("search index is unavailable")
	// ErrInvalidCursor means a pagination cursor is malformed or does not
	// match the index sort order.
	ErrInvalidCursor = errors.New("invalid search cursor")
	// ErrInvalidQuery means the query itself is not supported.
	ErrInvalidQuery = errors.New("invalid search query")
)

const (
	// defaultLimit is used when Query.Limit is not positive.
	defaultLimit = 50
	// boltTimeout bounds how long an index open waits for the underlying
	// file lock before reporting ErrLocked.
	boltTimeout = "250ms"
)

// Document is the indexable projection of a message. The caller assembles it
// from PostgreSQL metadata and the extracted body; the search index never
// treats it as the source of truth.
type Document struct {
	ID        string
	AccountID string
	MailboxID string

	Subject string
	Body    string

	// From, To, Cc and Bcc hold formatted addresses for display.
	From, To, Cc, Bcc []string
	// FromAddress, ToAddress, CcAddress and BccAddress hold concatenated
	// addresses that are matched by the sender and recipient filters.
	FromAddress, ToAddress, CcAddress, BccAddress string

	ReceivedAt time.Time
	SentAt     time.Time

	Unread         bool
	Marked         bool
	HasAttachments bool

	Size  int64
	Flags []string

	// IsArchive marks local archive copies so results can be labelled.
	IsArchive bool
}

// Query is a free-text search combined with structured filters. All filters
// are ANDed with the text query.
type Query struct {
	// Text is the free-text query. Empty text matches every document that
	// satisfies the filters.
	Text string
	// Mode is "subject" to search only subjects, or "all" (default) to
	// search subject, body and addresses.
	Mode string

	AccountID string
	MailboxID string

	Recipient string
	Sender    string

	// HasAttachment filters on attachment presence when set.
	HasAttachment *bool

	// Limit is the page size. Values <= 0 fall back to the default.
	Limit int
	// Cursor is an opaque NextCursor value from a previous Result.
	Cursor string
}

// Hit is one search result. The caller must load message metadata from
// PostgreSQL before rendering it.
type Hit struct {
	ID        string
	Score     float64
	Highlight string
	IsArchive bool
}

// Result is one page of search results.
type Result struct {
	Hits       []Hit
	NextCursor string
	Total      uint64
}

// Index is a single Bleve index handle. Reads may run concurrently; writes
// are serialized inside Bleve. Close releases the process and file locks.
type Index struct {
	mu     sync.RWMutex
	db     bleve.Index
	path   string
	dirKey string
	closed bool
}

var (
	openMu   sync.Mutex
	openDirs = map[string]struct{}{}
)

// Open opens the index stored at dir, creating it with the current mapping
// when it does not exist yet. Only one handle per directory may be open in
// this process; the Bleve lock guards other processes.
func Open(dir string) (*Index, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("%w: empty index directory", ErrUnavailable)
	}
	key, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve index path: %w", ErrUnavailable, err)
	}

	openMu.Lock()
	if _, exists := openDirs[key]; exists {
		openMu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrLocked, key)
	}
	openDirs[key] = struct{}{}
	openMu.Unlock()

	db, err := openOrCreate(key)
	if err != nil {
		openMu.Lock()
		delete(openDirs, key)
		openMu.Unlock()
		if isLockError(err) {
			return nil, fmt.Errorf("%w: %w", ErrLocked, err)
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	return &Index{db: db, path: key, dirKey: key}, nil
}

// isLockError reports whether Bleve failed because another process holds the
// index. The underlying bbolt timeout is currently the only error it reports
// for a held lock; bbolt is an indirect dependency, so its sentinel cannot be
// imported without changing go.mod.
func isLockError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}

func openOrCreate(dir string) (bleve.Index, error) {
	if _, err := os.Stat(filepath.Join(dir, "index_meta.json")); err == nil {
		return bleve.Open(dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}
	config := map[string]any{"bolt_timeout": boltTimeout}
	return bleve.NewUsing(dir, buildMapping(), scorch.Name, scorch.Name, config)
}

// Close releases the index. It is safe to call more than once. Every
// operation after Close reports ErrUnavailable.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true

	openMu.Lock()
	delete(openDirs, i.dirKey)
	openMu.Unlock()

	if i.db == nil {
		return nil
	}
	err := i.db.Close()
	i.db = nil
	if err != nil {
		return fmt.Errorf("search: close index: %w", err)
	}
	return nil
}

// Upsert indexes docs, replacing any document with the same ID. It is
// idempotent and safe to retry.
func (i *Index) Upsert(ctx context.Context, docs []Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(docs) == 0 {
		return nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.db == nil {
		return fmt.Errorf("%w: index is closed", ErrUnavailable)
	}

	batch := i.db.NewBatch()
	for _, doc := range docs {
		if doc.ID == "" {
			return errors.New("search: document id is required")
		}
		if err := batch.Index(doc.ID, doc.fields()); err != nil {
			return fmt.Errorf("search: index %q: %w", doc.ID, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := i.db.Batch(batch); err != nil {
		return fmt.Errorf("search: write batch: %w", err)
	}
	return nil
}

// Delete removes the documents with the given IDs. Missing IDs are ignored.
func (i *Index) Delete(ctx context.Context, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.db == nil {
		return fmt.Errorf("%w: index is closed", ErrUnavailable)
	}

	batch := i.db.NewBatch()
	for _, id := range ids {
		if id == "" {
			continue
		}
		batch.Delete(id)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := i.db.Batch(batch); err != nil {
		return fmt.Errorf("search: delete batch: %w", err)
	}
	return nil
}

// Search runs a query and returns one page of hits.
func (i *Index) Search(ctx context.Context, q Query) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	qry, err := buildQuery(q)
	if err != nil {
		return Result{}, err
	}

	order := sortOrder()
	var after []string
	if q.Cursor != "" {
		after, err = decodeCursor(q.Cursor)
		if err != nil {
			return Result{}, err
		}
		if len(after) != len(order) {
			return Result{}, fmt.Errorf("%w: cursor has %d values, want %d",
				ErrInvalidCursor, len(after), len(order))
		}
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.closed || i.db == nil {
		return Result{}, fmt.Errorf("%w: index is closed", ErrUnavailable)
	}

	req := bleve.NewSearchRequestOptions(qry, limit, 0, false)
	req.Sort = order
	req.Fields = []string{"is_archive"}
	req.Highlight = newHighlight()
	if after != nil {
		req.SetSearchAfter(after)
	}

	res, err := i.db.SearchInContext(ctx, req)
	if err != nil {
		return Result{}, fmt.Errorf("search: query: %w", err)
	}

	result := Result{
		Hits:  make([]Hit, 0, len(res.Hits)),
		Total: res.Total,
	}
	for _, match := range res.Hits {
		hit := Hit{
			ID:        match.ID,
			Score:     match.Score,
			Highlight: bestFragment(match.Fragments),
		}
		if archived, ok := match.Fields["is_archive"].(bool); ok {
			hit.IsArchive = archived
		}
		result.Hits = append(result.Hits, hit)
	}
	if len(res.Hits) == limit && len(result.Hits) > 0 {
		result.NextCursor, err = encodeCursor(res.Hits[len(res.Hits)-1].Sort)
		if err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

// Count reports the number of documents in the index.
func (i *Index) Count() (uint64, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.closed || i.db == nil {
		return 0, fmt.Errorf("%w: index is closed", ErrUnavailable)
	}
	count, err := i.db.DocCount()
	if err != nil {
		return 0, fmt.Errorf("search: count documents: %w", err)
	}
	return count, nil
}

// Reset removes every document and keeps the index handle usable.
func (i *Index) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.db == nil {
		return fmt.Errorf("%w: index is closed", ErrUnavailable)
	}

	if err := i.db.Close(); err != nil {
		i.db = nil
		return fmt.Errorf("search: close index for reset: %w", err)
	}
	i.db = nil

	if err := os.RemoveAll(i.path); err != nil {
		return fmt.Errorf("search: remove index for reset: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	db, err := openOrCreate(i.path)
	if err != nil {
		return fmt.Errorf("%w: recreate index: %w", ErrUnavailable, err)
	}
	i.db = db
	return nil
}

func (d Document) fields() map[string]any {
	return map[string]any{
		"message_id": d.ID,
		"account_id": d.AccountID,
		"mailbox_id": d.MailboxID,

		"subject": d.Subject,
		"body":    d.Body,

		"from":         d.From,
		"to":           d.To,
		"cc":           d.Cc,
		"bcc":          d.Bcc,
		"from.address": d.FromAddress,
		"to.address":   d.ToAddress,
		"cc.address":   d.CcAddress,
		"bcc.address":  d.BccAddress,

		"received_at": d.ReceivedAt,
		"sent_at":     d.SentAt,

		"unread":          d.Unread,
		"marked":          d.Marked,
		"has_attachments": d.HasAttachments,

		"size":       d.Size,
		"flags":      d.Flags,
		"is_archive": d.IsArchive,
	}
}
