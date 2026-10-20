package search

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2"
	blevesearch "github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"
)

// buildQuery turns a Query into a Bleve query. Free text and every structured
// filter are ANDed together.
func buildQuery(q Query) (query.Query, error) {
	parts := make([]query.Query, 0, 7)

	text := strings.TrimSpace(q.Text)
	switch q.Mode {
	case "", "all", "subject":
	default:
		return nil, fmt.Errorf("%w: unsupported mode %q", ErrInvalidQuery, q.Mode)
	}

	if text == "" {
		parts = append(parts, bleve.NewMatchAllQuery())
	} else {
		switch q.Mode {
		case "", "all":
			disjuncts := make([]query.Query, 0, 6)
			for _, field := range []struct {
				name  string
				boost float64
			}{
				{"subject", 3},
				{"from", 2},
				{"body", 1},
				{"to", 1},
				{"cc", 1},
				{"bcc", 1},
			} {
				mq := bleve.NewMatchQuery(text)
				mq.SetField(field.name)
				mq.SetBoost(field.boost)
				disjuncts = append(disjuncts, mq)
			}
			parts = append(parts, newAnyQuery(disjuncts...))
		case "subject":
			mq := bleve.NewMatchQuery(text)
			mq.SetField("subject")
			parts = append(parts, mq)
		}
	}

	if q.AccountID != "" {
		parts = append(parts, termQuery("account_id", q.AccountID))
	}
	if q.MailboxID != "" {
		parts = append(parts, termQuery("mailbox_id", q.MailboxID))
	}
	if q.Recipient != "" {
		parts = append(parts, phraseAnyQuery(q.Recipient,
			"to", "to.address", "cc", "cc.address", "bcc", "bcc.address"))
	}
	if q.Sender != "" {
		parts = append(parts, phraseAnyQuery(q.Sender, "from", "from.address"))
	}
	if q.HasAttachment != nil {
		bq := bleve.NewBoolFieldQuery(*q.HasAttachment)
		bq.SetField("has_attachments")
		parts = append(parts, bq)
	}

	if len(parts) == 1 {
		return parts[0], nil
	}
	return bleve.NewConjunctionQuery(parts...), nil
}

func termQuery(field, term string) *query.TermQuery {
	tq := bleve.NewTermQuery(term)
	tq.SetField(field)
	return tq
}

// phraseAnyQuery matches the whole address string against any of the fields.
// Phrase matching keeps every analyzed token adjacent, so a domain alone does
// not match a different recipient on the same domain.
func phraseAnyQuery(text string, fields ...string) *query.DisjunctionQuery {
	disjuncts := make([]query.Query, 0, len(fields))
	for _, field := range fields {
		mq := bleve.NewMatchPhraseQuery(text)
		mq.SetField(field)
		disjuncts = append(disjuncts, mq)
	}
	return newAnyQuery(disjuncts...)
}

// newAnyQuery builds a disjunction that matches at least one clause. Bleve
// defaults DisjunctionQuery.Min to zero, which matches every document.
func newAnyQuery(disjuncts ...query.Query) *query.DisjunctionQuery {
	dq := bleve.NewDisjunctionQuery(disjuncts...)
	dq.SetMin(1)
	return dq
}

// sortOrder is the default result order: relevance descending, then newest
// first, then message ID for a stable total order.
func sortOrder() blevesearch.SortOrder {
	return blevesearch.SortOrder{
		&scoreSort{desc: true},
		&blevesearch.SortField{Field: "received_at", Desc: true},
		&blevesearch.SortField{Field: "message_id"},
	}
}

// scoreSort behaves like Bleve's "_score" sort but writes the numeric score
// as the sort value. Bleve's built-in score sort stores the literal "_score",
// which cannot be replayed through SearchAfter.
type scoreSort struct {
	desc bool
}

func (s *scoreSort) UpdateVisitor(string, []byte) {}

func (s *scoreSort) Value(match *blevesearch.DocumentMatch) string {
	return strconv.FormatFloat(match.Score, 'g', -1, 64)
}

func (s *scoreSort) DecodeValue(value string) string { return value }

func (s *scoreSort) Descending() bool { return s.desc }

func (s *scoreSort) RequiresDocID() bool { return false }

func (s *scoreSort) RequiresScoring() bool { return true }

func (s *scoreSort) RequiresFields() []string { return nil }

func (s *scoreSort) Reverse() { s.desc = !s.desc }

func (s *scoreSort) Copy() blevesearch.SearchSort {
	copied := *s
	return &copied
}

func (s *scoreSort) MarshalJSON() ([]byte, error) {
	if s.desc {
		return json.Marshal("-_score")
	}
	return json.Marshal("_score")
}
