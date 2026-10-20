package search

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/standard"
	"github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	"github.com/blevesearch/bleve/v2/mapping"
)

func newTestIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := Open(filepath.Join(t.TempDir(), "mailquay.bleve"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := idx.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return idx
}

func mustUpsert(t *testing.T, idx *Index, docs ...Document) {
	t.Helper()
	if err := idx.Upsert(context.Background(), docs); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

func mustSearch(t *testing.T, idx *Index, q Query) Result {
	t.Helper()
	res, err := idx.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	return res
}

func hitIDs(res Result) []string {
	ids := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

func sortedIDs(res Result) []string {
	ids := hitIDs(res)
	sort.Strings(ids)
	return ids
}

func docIndices(res Result) map[string]Hit {
	byID := make(map[string]Hit, len(res.Hits))
	for _, hit := range res.Hits {
		byID[hit.ID] = hit
	}
	return byID
}

func assertIDs(t *testing.T, res Result, want ...string) {
	t.Helper()
	got := sortedIDs(res)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("hit IDs = %v, want %v", got, want)
	}
}

func TestMapping(t *testing.T) {
	if MappingVersion() != 1 {
		t.Fatalf("MappingVersion() = %d, want 1", MappingVersion())
	}

	built, ok := buildMapping().(*mapping.IndexMappingImpl)
	if !ok {
		t.Fatalf("buildMapping returned %T, want *mapping.IndexMappingImpl", buildMapping())
	}
	if err := built.Validate(); err != nil {
		t.Fatalf("mapping validation: %v", err)
	}

	field := func(name string) *mapping.FieldMapping {
		t.Helper()
		sub, ok := built.DefaultMapping.Properties[name]
		if !ok || len(sub.Fields) == 0 {
			t.Fatalf("field %q is not mapped", name)
		}
		return sub.Fields[0]
	}

	subject := field("subject")
	if subject.Type != "text" || subject.Analyzer != cjk.AnalyzerName {
		t.Fatalf("subject mapping = type %q analyzer %q, want text/cjk", subject.Type, subject.Analyzer)
	}
	if !subject.Store || !subject.IncludeTermVectors {
		t.Fatal("subject must be stored with term vectors for highlighting")
	}

	body := field("body")
	if body.Type != "text" || body.Analyzer != cjk.AnalyzerName || !body.Store {
		t.Fatalf("body mapping = %+v", body)
	}

	messageID := field("message_id")
	if messageID.Type != "text" || messageID.Analyzer != keyword.Name {
		t.Fatalf("message_id mapping = type %q analyzer %q", messageID.Type, messageID.Analyzer)
	}
	if !messageID.Store || !messageID.DocValues {
		t.Fatal("message_id must be stored with doc values")
	}

	for _, name := range []string{"account_id", "mailbox_id"} {
		fm := field(name)
		if fm.Type != "text" || fm.Analyzer != keyword.Name || !fm.DocValues {
			t.Fatalf("%s mapping = %+v", name, fm)
		}
	}

	for _, name := range []string{"from", "to", "cc", "bcc"} {
		fm := field(name)
		if fm.Type != "text" || fm.Analyzer != cjk.AnalyzerName {
			t.Fatalf("%s mapping = type %q analyzer %q", name, fm.Type, fm.Analyzer)
		}
		if !fm.IncludeTermVectors {
			t.Fatalf("%s must include term vectors for phrase filters", name)
		}
		addr := field(name + ".address")
		if addr.Type != "text" || addr.Analyzer != standard.Name {
			t.Fatalf("%s.address mapping = type %q analyzer %q", name, addr.Type, addr.Analyzer)
		}
		if !addr.IncludeTermVectors {
			t.Fatalf("%s.address must include term vectors for phrase filters", name)
		}
	}

	for _, name := range []string{"received_at", "sent_at"} {
		fm := field(name)
		if fm.Type != "datetime" || !fm.DocValues {
			t.Fatalf("%s mapping = type %q docvalues %v", name, fm.Type, fm.DocValues)
		}
	}

	for _, name := range []string{"unread", "marked", "has_attachments", "is_archive"} {
		if fm := field(name); fm.Type != "boolean" {
			t.Fatalf("%s mapping = type %q, want boolean", name, fm.Type)
		}
	}
	if !field("is_archive").Store {
		t.Fatal("is_archive must be stored so hits can be labelled")
	}

	if fm := field("size"); fm.Type != "number" {
		t.Fatalf("size mapping = type %q, want number", fm.Type)
	}
	if fm := field("flags"); fm.Type != "text" || fm.Analyzer != keyword.Name {
		t.Fatalf("flags mapping = type %q analyzer %q", fm.Type, fm.Analyzer)
	}
}

func TestOpenCloseAndLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index")

	idx, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if count, err := idx.Count(); err != nil || count != 0 {
		t.Fatalf("Count() = %d, %v; want 0, nil", count, err)
	}

	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open error = %v, want ErrLocked", err)
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := idx.Count(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Count after Close error = %v, want ErrUnavailable", err)
	}
	if _, err := idx.Search(context.Background(), Query{Text: "anything"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Search after Close error = %v, want ErrUnavailable", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened: %v", err)
	}
}

func TestEmptyDirectoryIsUnavailable(t *testing.T) {
	if _, err := Open("   "); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Open(blank) error = %v, want ErrUnavailable", err)
	}
}

// TestCrossProcessLock runs the lock probe in a child process so the Bleve
// file lock, rather than the in-process guard, is exercised.
func TestCrossProcessLock(t *testing.T) {
	if dir := os.Getenv("MAILQUAY_SEARCH_LOCK_DIR"); dir != "" {
		idx, err := Open(dir)
		if err == nil {
			_ = idx.Close()
			os.Exit(0)
		}
		if errors.Is(err, ErrLocked) {
			os.Exit(3)
		}
		fmt.Fprintln(os.Stderr, "unexpected open error:", err)
		os.Exit(2)
	}

	dir := filepath.Join(t.TempDir(), "index")
	idx, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := idx.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrossProcessLock$")
	cmd.Env = append(os.Environ(), "MAILQUAY_SEARCH_LOCK_DIR="+dir)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child process error = %v, output:\n%s", err, output)
	}
	if exitErr.ExitCode() != 3 {
		t.Fatalf("child exit code = %d, want 3 (ErrLocked); output:\n%s", exitErr.ExitCode(), output)
	}
}

func TestUpsertIsIdempotentAndDelete(t *testing.T) {
	idx := newTestIndex(t)
	received := time.Date(2025, time.March, 1, 12, 0, 0, 0, time.UTC)

	doc := Document{
		ID:          "message-1",
		AccountID:   "account-1",
		MailboxID:   "inbox",
		Subject:     "first subject",
		Body:        "original body",
		From:        []string{"Alice <alice@example.com>"},
		FromAddress: "alice@example.com",
		To:          []string{"Bob <bob@example.com>"},
		ToAddress:   "bob@example.com",
		ReceivedAt:  received,
		Size:        1024,
		Flags:       []string{"\\Seen"},
	}
	mustUpsert(t, idx, doc)
	if count, err := idx.Count(); err != nil || count != 1 {
		t.Fatalf("Count() = %d, %v; want 1, nil", count, err)
	}

	doc.Subject = "second subject"
	mustUpsert(t, idx, doc)
	if count, err := idx.Count(); err != nil || count != 1 {
		t.Fatalf("Count after upsert = %d, %v; want 1, nil", count, err)
	}
	assertIDs(t, mustSearch(t, idx, Query{Text: "second"}), "message-1")
	if res := mustSearch(t, idx, Query{Text: "first"}); len(res.Hits) != 0 {
		t.Fatalf("stale subject still matches: %v", hitIDs(res))
	}

	mustUpsert(t, idx,
		Document{ID: "message-2", Subject: "second message", ReceivedAt: received},
		Document{ID: "message-3", Subject: "third message", ReceivedAt: received},
	)
	if count, err := idx.Count(); err != nil || count != 3 {
		t.Fatalf("Count() = %d, %v; want 3, nil", count, err)
	}

	if err := idx.Delete(context.Background(), []string{"message-1", "missing"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if count, err := idx.Count(); err != nil || count != 2 {
		t.Fatalf("Count after delete = %d, %v; want 2, nil", count, err)
	}
	assertIDs(t, mustSearch(t, idx, Query{Text: "message"}), "message-2", "message-3")

	if err := idx.Upsert(context.Background(), []Document{{Subject: "no id"}}); err == nil {
		t.Fatal("Upsert with an empty ID must fail")
	}
}

func TestCJKAnalysisAndHighlight(t *testing.T) {
	idx := newTestIndex(t)
	received := time.Date(2025, time.April, 2, 9, 0, 0, 0, time.UTC)

	mustUpsert(t, idx,
		Document{
			ID:         "english",
			Subject:    "Quarterly report",
			Body:       "This is the English body",
			ReceivedAt: received,
		},
		Document{
			ID:         "chinese-body",
			Subject:    "项目进度更新",
			Body:       "本周完成了中文搜索的测试",
			ReceivedAt: received.Add(-time.Hour),
		},
		Document{
			ID:         "chinese-subject",
			Subject:    "中文邮件标题测试",
			Body:       "正文不包含关键词",
			ReceivedAt: received.Add(-2 * time.Hour),
		},
	)

	all := mustSearch(t, idx, Query{Text: "中文", Mode: "all"})
	assertIDs(t, all, "chinese-body", "chinese-subject")
	for _, hit := range all.Hits {
		if hit.Highlight == "" {
			t.Fatalf("hit %s has no highlight", hit.ID)
		}
		if !strings.Contains(hit.Highlight, "中文") {
			t.Fatalf("hit %s highlight %q does not contain the term", hit.ID, hit.Highlight)
		}
	}

	// The subject hit outranks the body-only hit because subject^3.
	if len(all.Hits) == 2 && all.Hits[0].ID != "chinese-subject" {
		t.Fatalf("subject match should rank first, got %v", hitIDs(all))
	}

	subjectOnly := mustSearch(t, idx, Query{Text: "中文", Mode: "subject"})
	assertIDs(t, subjectOnly, "chinese-subject")

	english := mustSearch(t, idx, Query{Text: "English body", Mode: "all"})
	assertIDs(t, english, "english")
	if english.Hits[0].Highlight == "" {
		t.Fatal("english hit has no highlight")
	}

	if _, err := idx.Search(context.Background(), Query{Text: "x", Mode: "bogus"}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("invalid mode error = %v, want ErrInvalidQuery", err)
	}
}

func TestStructuredFilters(t *testing.T) {
	idx := newTestIndex(t)
	received := time.Date(2025, time.May, 10, 8, 0, 0, 0, time.UTC)

	mustUpsert(t, idx,
		Document{
			ID: "d1", AccountID: "a1", MailboxID: "inbox1",
			Subject: "Team sync", From: []string{"Alice <alice@example.com>"},
			FromAddress: "alice@example.com", To: []string{"Bob <bob@example.com>"},
			ToAddress: "bob@example.com", HasAttachments: true, Unread: true,
			ReceivedAt: received,
		},
		Document{
			ID: "d2", AccountID: "a1", MailboxID: "inbox2",
			Subject: "Plan review", From: []string{"Carol <carol@example.com>"},
			FromAddress: "carol@example.com", To: []string{"Alice <alice@example.com>"},
			ToAddress: "alice@example.com", IsArchive: true,
			ReceivedAt: received.Add(-time.Hour),
		},
		Document{
			ID: "d3", AccountID: "a2", MailboxID: "inbox1",
			Subject: "Team sync notes", From: []string{"Dave <dave@example.com>"},
			FromAddress: "dave@example.com", To: []string{"Team <team@example.com>"},
			ToAddress: "team@example.com", Cc: []string{"Carol <carol@example.com>"},
			CcAddress: "carol@example.com", HasAttachments: true,
			ReceivedAt: received.Add(-2 * time.Hour),
		},
		Document{
			ID: "d4", AccountID: "a2", MailboxID: "inbox2",
			Subject: "Private note", From: []string{"Eve <eve@example.com>"},
			FromAddress: "eve@example.com", To: []string{"Dave <dave@example.com>"},
			ToAddress: "dave@example.com", Bcc: []string{"Frank <frank@example.com>"},
			BccAddress: "frank@example.com",
			ReceivedAt: received.Add(-3 * time.Hour),
		},
	)

	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"account", Query{AccountID: "a1"}, []string{"d1", "d2"}},
		{"mailbox", Query{MailboxID: "inbox1"}, []string{"d1", "d3"}},
		{"account and mailbox", Query{AccountID: "a1", MailboxID: "inbox1"}, []string{"d1"}},
		{"sender", Query{Sender: "alice@example.com"}, []string{"d1"}},
		{"recipient to", Query{Recipient: "bob@example.com"}, []string{"d1"}},
		{"recipient cc", Query{Recipient: "carol@example.com"}, []string{"d3"}},
		{"recipient bcc", Query{Recipient: "frank@example.com"}, []string{"d4"}},
		{"text and filter", Query{Text: "Team sync", AccountID: "a1"}, []string{"d1"}},
		{"empty text with filters", Query{AccountID: "a2", MailboxID: "inbox1"}, []string{"d3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertIDs(t, mustSearch(t, idx, tc.q), tc.want...)
		})
	}

	withAttachments := true
	withoutAttachments := false
	assertIDs(t, mustSearch(t, idx, Query{HasAttachment: &withAttachments}), "d1", "d3")
	assertIDs(t, mustSearch(t, idx, Query{HasAttachment: &withoutAttachments}), "d2", "d4")
	assertIDs(t, mustSearch(t, idx, Query{AccountID: "a2", HasAttachment: &withAttachments}), "d3")

	if empty := mustSearch(t, idx, Query{}); empty.Total != 4 || len(empty.Hits) != 4 {
		t.Fatalf("empty query returned total %d with %d hits, want 4", empty.Total, len(empty.Hits))
	}

	archived := docIndices(mustSearch(t, idx, Query{AccountID: "a1"}))
	if !archived["d2"].IsArchive {
		t.Fatalf("d2 hit IsArchive = false, want true")
	}
	if archived["d1"].IsArchive {
		t.Fatalf("d1 hit IsArchive = true, want false")
	}
}

func TestPagination(t *testing.T) {
	idx := newTestIndex(t)
	base := time.Date(2025, time.June, 1, 12, 0, 0, 0, time.UTC)

	const total = 25
	docs := make([]Document, 0, total)
	for i := range total {
		docs = append(docs, Document{
			ID:         fmt.Sprintf("msg-%02d", i),
			AccountID:  "account-1",
			MailboxID:  "inbox",
			Subject:    "pagination test",
			Body:       "same body for every message",
			ReceivedAt: base.Add(-time.Duration(i) * time.Hour),
		})
	}
	mustUpsert(t, idx, docs...)

	seen := make([]string, 0, total)
	cursor := ""
	pages := 0
	for {
		res := mustSearch(t, idx, Query{Text: "pagination", Limit: 10, Cursor: cursor})
		pages++
		if res.Total != total {
			t.Fatalf("page %d Total = %d, want %d", pages, res.Total, total)
		}
		seen = append(seen, hitIDs(res)...)
		if res.NextCursor == "" {
			break
		}
		if cursor != "" && res.NextCursor == cursor {
			t.Fatalf("page %d repeated cursor %q", pages, cursor)
		}
		cursor = res.NextCursor
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}

	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
	if len(seen) != total {
		t.Fatalf("collected %d hits, want %d", len(seen), total)
	}
	unique := map[string]struct{}{}
	for _, id := range seen {
		if _, dup := unique[id]; dup {
			t.Fatalf("duplicate hit %q across pages", id)
		}
		unique[id] = struct{}{}
	}
	for i := range total {
		want := fmt.Sprintf("msg-%02d", i)
		if seen[i] != want {
			t.Fatalf("hit %d = %q, want %q (received_at desc order)", i, seen[i], want)
		}
	}
}

func TestPaginationTieBreaksByMessageID(t *testing.T) {
	idx := newTestIndex(t)
	received := time.Date(2025, time.June, 2, 12, 0, 0, 0, time.UTC)

	// Document IDs are also indexed as message_id, so the tie-break order is
	// doc-a, doc-b, doc-c, doc-d regardless of insertion order.
	mustUpsert(t, idx,
		Document{ID: "doc-c", Subject: "tie break", ReceivedAt: received},
		Document{ID: "doc-a", Subject: "tie break", ReceivedAt: received},
		Document{ID: "doc-d", Subject: "tie break", ReceivedAt: received},
		Document{ID: "doc-b", Subject: "tie break", ReceivedAt: received},
	)

	seen := make([]string, 0, 4)
	cursor := ""
	for {
		res := mustSearch(t, idx, Query{Text: "tie", Limit: 2, Cursor: cursor})
		seen = append(seen, hitIDs(res)...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if fmt.Sprint(seen) != fmt.Sprint([]string{"doc-a", "doc-b", "doc-c", "doc-d"}) {
		t.Fatalf("hits = %v, want message_id ascending", seen)
	}
}

func TestPaginationByScore(t *testing.T) {
	idx := newTestIndex(t)
	received := time.Date(2025, time.June, 3, 12, 0, 0, 0, time.UTC)

	mustUpsert(t, idx,
		Document{ID: "score-high", Subject: "needle in the haystack", Body: "short", ReceivedAt: received},
		Document{ID: "score-mid", Subject: "report", Body: strings.Repeat("needle ", 20), ReceivedAt: received},
		Document{ID: "score-low", Subject: "report", Body: "needle", ReceivedAt: received},
	)

	all := mustSearch(t, idx, Query{Text: "needle", Limit: 10})
	if len(all.Hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(all.Hits))
	}
	for i := 1; i < len(all.Hits); i++ {
		if all.Hits[i-1].Score <= all.Hits[i].Score {
			t.Fatalf("scores are not strictly descending: %v", all.Hits)
		}
	}
	want := hitIDs(all)

	paged := make([]string, 0, len(want))
	cursor := ""
	for {
		res := mustSearch(t, idx, Query{Text: "needle", Limit: 1, Cursor: cursor})
		paged = append(paged, hitIDs(res)...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
		if len(paged) > len(want) {
			t.Fatalf("pagination returned more hits than the unpaginated query: %v", paged)
		}
	}
	if fmt.Sprint(paged) != fmt.Sprint(want) {
		t.Fatalf("paged order = %v, want score order %v", paged, want)
	}
}

func TestInvalidCursor(t *testing.T) {
	idx := newTestIndex(t)
	mustUpsert(t, idx, Document{ID: "m1", Subject: "cursor test"})

	if _, err := idx.Search(context.Background(), Query{Text: "cursor", Cursor: "not base64!"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad cursor error = %v, want ErrInvalidCursor", err)
	}

	wrongLength := base64.RawURLEncoding.EncodeToString([]byte(`["only-one"]`))
	if _, err := idx.Search(context.Background(), Query{Text: "cursor", Cursor: wrongLength}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("short cursor error = %v, want ErrInvalidCursor", err)
	}

	empty := base64.RawURLEncoding.EncodeToString([]byte(`null`))
	if _, err := idx.Search(context.Background(), Query{Text: "cursor", Cursor: empty}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("null cursor error = %v, want ErrInvalidCursor", err)
	}
}

func TestReset(t *testing.T) {
	idx := newTestIndex(t)
	received := time.Date(2025, time.July, 1, 12, 0, 0, 0, time.UTC)
	mustUpsert(t, idx,
		Document{ID: "m1", Subject: "reset one", ReceivedAt: received},
		Document{ID: "m2", Subject: "reset two", ReceivedAt: received},
		Document{ID: "m3", Subject: "reset three", ReceivedAt: received},
	)
	if count, err := idx.Count(); err != nil || count != 3 {
		t.Fatalf("Count() = %d, %v; want 3, nil", count, err)
	}

	if err := idx.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if count, err := idx.Count(); err != nil || count != 0 {
		t.Fatalf("Count after Reset = %d, %v; want 0, nil", count, err)
	}

	mustUpsert(t, idx, Document{ID: "m4", Subject: "after reset", ReceivedAt: received})
	assertIDs(t, mustSearch(t, idx, Query{Text: "reset"}), "m4")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := idx.Reset(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reset with canceled context = %v, want context.Canceled", err)
	}
}

func TestInvalidMode(t *testing.T) {
	idx := newTestIndex(t)
	mustUpsert(t, idx, Document{ID: "m1", AccountID: "a1", Subject: "mode test"})

	for _, q := range []Query{
		{Text: "mode", Mode: "bogus"},
		{Mode: "bogus", AccountID: "a1"},
	} {
		if _, err := idx.Search(context.Background(), q); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("Search(%+v) error = %v, want ErrInvalidQuery", q, err)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	idx := newTestIndex(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := idx.Upsert(ctx, []Document{{ID: "m1"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upsert canceled error = %v", err)
	}
	if err := idx.Delete(ctx, []string{"m1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete canceled error = %v", err)
	}
	if _, err := idx.Search(ctx, Query{Text: "anything"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search canceled error = %v", err)
	}
}
