package search

import (
	"strings"

	"github.com/blevesearch/bleve/v2"
	blevesearch "github.com/blevesearch/bleve/v2/search"
)

const highlightOpenTag = "<mark>"

// newHighlight requests fragments for the fields rendered in result rows.
func newHighlight() *bleve.HighlightRequest {
	highlight := bleve.NewHighlight()
	highlight.AddField("subject")
	highlight.AddField("body")
	return highlight
}

// bestFragment picks the fragment with the most highlighted terms. Ties go
// to the subject because it is what the result list shows.
func bestFragment(fragments blevesearch.FieldFragmentMap) string {
	best := ""
	bestTags := -1
	for _, field := range []string{"subject", "body"} {
		for _, fragment := range fragments[field] {
			tags := strings.Count(fragment, highlightOpenTag)
			if tags > bestTags {
				best = fragment
				bestTags = tags
			}
		}
	}
	return best
}
