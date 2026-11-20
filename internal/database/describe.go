package database

import (
	"encoding/json"

	"github.com/swqa7697/data-mate/internal/contracts"
)

// AddCatalogRow collects a row from a catalog ordered by schema. An empty kind
// records an empty schema. Drivers translate native object kinds before calling.
func (d *DatabaseDescription) AddCatalogRow(budget *MetadataBudget, schema, kind, name, table string) error {
	if len(d.Schemas) == 0 || d.Schemas[len(d.Schemas)-1].Name != schema {
		entry := SchemaDescription{
			Name: schema, Tables: []RelationName{}, Enums: []CatalogName{},
			Sequences: []CatalogName{}, Indexes: []IndexName{}, Functions: []CatalogName{},
		}
		if err := appendCatalogEntry(budget, &d.Schemas, entry); err != nil {
			return err
		}
	}
	s := &d.Schemas[len(d.Schemas)-1]
	switch kind {
	case "":
		return nil
	case "enum":
		return appendCatalogEntry(budget, &s.Enums, CatalogName{Name: name})
	case "sequence":
		return appendCatalogEntry(budget, &s.Sequences, CatalogName{Name: name})
	case "index":
		return appendCatalogEntry(budget, &s.Indexes, IndexName{Name: name, Table: table})
	case "function":
		return appendCatalogEntry(budget, &s.Functions, CatalogName{Name: name})
	default:
		return appendCatalogEntry(budget, &s.Tables, RelationName{Name: name, Kind: kind})
	}
}

func appendCatalogEntry[T any](budget *MetadataBudget, entries *[]T, entry T) error {
	if err := budget.Add(entry); err != nil {
		return err
	}
	*entries = append(*entries, entry)
	return nil
}

// CheckSize checks the complete CLI document, including its header and the
// separators between entries. The CLI returns a single JSON representation.
func (d *DatabaseDescription) CheckSize(limit int) error {
	if limit == 0 {
		return nil
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(encoded) > limit {
		return Fail(contracts.ResourceLimit, "database description exceeds profile result-byte limit", false)
	}
	return nil
}
