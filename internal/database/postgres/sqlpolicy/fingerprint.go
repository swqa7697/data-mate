package sqlpolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

const maxCatalogBytes = 2 << 20
const fingerprintDomain = "data-mate-catalog-v1:"

// CatalogFingerprintSQL projects the same audited catalog as CatalogSQL, but
// returns only its UTF-8 byte count and SHA-256 fingerprint. The size guard is
// evaluated before normalization/hashing. No live authorization is cached.
var CatalogFingerprintSQL = `WITH document AS MATERIALIZED (
 SELECT payload, pg_catalog.octet_length(pg_catalog.convert_to(payload, 'UTF8')) AS catalog_bytes
 FROM (` + CatalogSQL + `) AS source(payload)
)
SELECT catalog_bytes, CASE WHEN catalog_bytes BETWEEN 1 AND ` + strconv.Itoa(maxCatalogBytes) + ` THEN (
 SELECT pg_catalog.sha256(pg_catalog.convert_to('` + fingerprintDomain + `' || ($1::integer)::text || ':' ||
  pg_catalog.jsonb_object_agg(category.key, ordered.records)::text, 'UTF8'))
 FROM pg_catalog.jsonb_each(document.payload::jsonb) AS category
 CROSS JOIN LATERAL (
  SELECT pg_catalog.jsonb_agg(record ORDER BY record::text COLLATE "C") AS records
  FROM pg_catalog.jsonb_array_elements(category.value) AS entries(record)
 ) AS ordered
) END AS fingerprint FROM document`

// CatalogFingerprint returns a copy of the immutable reviewed fingerprint.
// Calling this before catalog access rejects unsupported majors locally.
func CatalogFingerprint(major int) ([sha256.Size]byte, error) {
	p, err := policyFor(major)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return p.fingerprint, nil
}

// VerifyCatalogFingerprint checks one fresh compact response. Byte length is a
// resource bound, not an identity: record order and JSON spelling are irrelevant.
func VerifyCatalogFingerprint(major int, size int64, actual []byte) error {
	expected, err := CatalogFingerprint(major)
	if err != nil {
		return err
	}
	if size > maxCatalogBytes {
		return database.Fail(contracts.ResourceLimit, "PostgreSQL catalog exceeds limit", false)
	}
	if size <= 0 || len(actual) != sha256.Size || !bytes.Equal(expected[:], actual) {
		return unsupported()
	}
	return nil
}

// fingerprintCatalog is called only after strict typed catalog validation. Keep
// normalization separate from validation so duplicate keys/identities cannot be
// hidden by JSON decoding or sorting. Only top-level record arrays are sorted;
// function argument/configuration arrays retain their meaningful order.
func fingerprintCatalog(major int, raw []byte) ([sha256.Size]byte, error) {
	var doc map[string][]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		return [sha256.Size]byte{}, unsupported()
	}
	ordered := make(map[string]any, len(doc))
	for name, records := range doc {
		encoded := make([]string, len(records))
		for i, record := range records {
			b, err := appendCatalogJSON(nil, record)
			if err != nil {
				return [sha256.Size]byte{}, err
			}
			encoded[i] = string(b)
		}
		slices.Sort(encoded)
		rows := make([]any, len(encoded))
		for i, b := range encoded {
			rows[i] = json.RawMessage(b)
		}
		ordered[name] = rows
	}
	prefix := fingerprintDomain + strconv.Itoa(major) + ":"
	b, err := appendCatalogJSON([]byte(prefix), ordered)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// appendCatalogJSON matches PostgreSQL 16/18 JSONB text for the manifest's
// strings, integers, booleans, nulls, arrays and objects. JSONB orders object keys
// by UTF-8 byte length, then byte value, and uses a space after commas/colons.
// Unlike encoding/json it does not escape HTML or U+2028/U+2029. Owned database
// regressions verify this encoding against PostgreSQL, not just Go round trips.
func appendCatalogJSON(dst []byte, value any) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		return strconv.AppendBool(dst, v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return nil, unsupported()
		}
		return strconv.AppendInt(dst, n, 10), nil
	case string:
		if !utf8.ValidString(v) || strings.IndexByte(v, 0) >= 0 {
			return nil, unsupported()
		}
		dst = append(dst, '"')
		for _, b := range []byte(v) {
			switch b {
			case '"', '\\':
				dst = append(dst, '\\', b)
			case '\b':
				dst = append(dst, `\b`...)
			case '\f':
				dst = append(dst, `\f`...)
			case '\n':
				dst = append(dst, `\n`...)
			case '\r':
				dst = append(dst, `\r`...)
			case '\t':
				dst = append(dst, `\t`...)
			default:
				if b < 0x20 {
					const hex = "0123456789abcdef"
					dst = append(dst, '\\', 'u', '0', '0', hex[b>>4], hex[b&15])
				} else {
					dst = append(dst, b)
				}
			}
		}
		return append(dst, '"'), nil
	case json.RawMessage: // Already normalized records, never unvalidated input.
		return append(dst, v...), nil
	case []any:
		dst = append(dst, '[')
		for i, item := range v {
			if i > 0 {
				dst = append(dst, ", "...)
			}
			var err error
			dst, err = appendCatalogJSON(dst, item)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int {
			if len(a) != len(b) {
				return len(a) - len(b)
			}
			return strings.Compare(a, b)
		})
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ", "...)
			}
			var err error
			dst, err = appendCatalogJSON(dst, k)
			if err != nil {
				return nil, err
			}
			dst = append(dst, ": "...)
			dst, err = appendCatalogJSON(dst, v[k])
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	default:
		return nil, unsupported()
	}
}
