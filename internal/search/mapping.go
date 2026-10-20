package search

import (
	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/standard"
	"github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	"github.com/blevesearch/bleve/v2/mapping"
)

// mappingVersion identifies the field layout and analyzers below. Bump it
// whenever a field or analyzer changes so the caller can rebuild the index.
const mappingVersion = 1

// MappingVersion returns the current index mapping version.
func MappingVersion() uint { return mappingVersion }

func buildMapping() mapping.IndexMapping {
	indexMapping := bleve.NewIndexMapping()

	doc := bleve.NewDocumentMapping()
	doc.Dynamic = false

	doc.AddFieldMappingsAt("message_id", keywordFieldMapping(true, true))
	doc.AddFieldMappingsAt("account_id", keywordFieldMapping(false, true))
	doc.AddFieldMappingsAt("mailbox_id", keywordFieldMapping(false, true))

	doc.AddFieldMappingsAt("subject", textFieldMapping(cjk.AnalyzerName, true))
	doc.AddFieldMappingsAt("body", textFieldMapping(cjk.AnalyzerName, true))

	for _, name := range []string{"from", "to", "cc", "bcc"} {
		doc.AddFieldMappingsAt(name, textFieldMapping(cjk.AnalyzerName, false))
		doc.AddFieldMappingsAt(name+".address", textFieldMapping(standard.Name, false))
	}

	doc.AddFieldMappingsAt("received_at", dateTimeFieldMapping())
	doc.AddFieldMappingsAt("sent_at", dateTimeFieldMapping())

	doc.AddFieldMappingsAt("unread", boolFieldMapping(false))
	doc.AddFieldMappingsAt("marked", boolFieldMapping(false))
	doc.AddFieldMappingsAt("has_attachments", boolFieldMapping(false))
	doc.AddFieldMappingsAt("is_archive", boolFieldMapping(true))

	doc.AddFieldMappingsAt("size", numericFieldMapping())
	doc.AddFieldMappingsAt("flags", keywordFieldMapping(false, false))

	indexMapping.DefaultMapping = doc
	return indexMapping
}

func textFieldMapping(analyzer string, store bool) *mapping.FieldMapping {
	fm := bleve.NewTextFieldMapping()
	fm.Analyzer = analyzer
	fm.Store = store
	fm.IncludeTermVectors = true
	fm.IncludeInAll = false
	fm.DocValues = false
	return fm
}

func keywordFieldMapping(store, docValues bool) *mapping.FieldMapping {
	fm := bleve.NewKeywordFieldMapping()
	fm.Store = store
	fm.IncludeTermVectors = false
	fm.IncludeInAll = false
	fm.DocValues = docValues
	return fm
}

func dateTimeFieldMapping() *mapping.FieldMapping {
	fm := bleve.NewDateTimeFieldMapping()
	fm.Store = false
	fm.IncludeInAll = false
	fm.DocValues = true
	return fm
}

func boolFieldMapping(store bool) *mapping.FieldMapping {
	fm := bleve.NewBooleanFieldMapping()
	fm.Store = store
	fm.IncludeInAll = false
	fm.DocValues = false
	return fm
}

func numericFieldMapping() *mapping.FieldMapping {
	fm := bleve.NewNumericFieldMapping()
	fm.Store = false
	fm.IncludeInAll = false
	fm.DocValues = true
	return fm
}
