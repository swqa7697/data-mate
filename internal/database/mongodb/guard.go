package mongodb

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// maxDecimalText bounds $numberDecimal text, whose parser does quadratic work.
const maxDecimalText = 64

func invalidDocument() error {
	return database.Fail(contracts.InvalidArgument, "invalid MongoDB Extended JSON document", false)
}

func invalidNames() error {
	return database.Fail(contracts.InvalidArgument, "invalid MongoDB database or collection name", false)
}

func scriptRejected() error {
	return database.Fail(contracts.QueryUnsupported, "server-side JavaScript and internal operators are not supported", false)
}

func invalidStage() error {
	return database.Fail(contracts.InvalidArgument, "each aggregation stage must be a document with one stage operator", false)
}

func invalidSubPipeline() error {
	return database.Fail(contracts.InvalidArgument, "aggregation sub-pipelines must be arrays of stage documents", false)
}

// checkNames validates a database and collection before any input is parsed.
func checkNames(db, collection string) error {
	if config.MongoDBDatabaseName(db) != nil || !validCollectionName(db, collection) {
		return invalidNames()
	}
	return nil
}

// parseDocument converts one agent Extended JSON object, canonical or relaxed,
// into ordered BSON. Input text is never echoed in errors. An optional empty
// input means the document was omitted.
func parseDocument(raw json.RawMessage, optional bool) (bson.Raw, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 && optional {
		return nil, nil
	}
	if contracts.JSONMessage(trimmed) != nil || len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalidDocument()
	}
	if err := checkTokens(trimmed); err != nil {
		return nil, err
	}
	var doc bson.Raw
	if err := bson.UnmarshalExtJSON(trimmed, false, &doc); err != nil {
		return nil, invalidDocument()
	}
	return doc, nil
}

// checkTokens rejects JSON integers that relaxed Extended JSON would silently
// turn into doubles, and $numberDecimal text long enough to make parsing
// expensive, in one streaming pass.
func checkTokens(raw []byte) error {
	type frame struct{ object, key bool }
	var stack []frame
	key := ""
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	for {
		token, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return invalidDocument()
		}
		top := len(stack) - 1
		if token == json.Delim('}') || token == json.Delim(']') {
			stack = stack[:top]
			if top > 0 && stack[top-1].object {
				stack[top-1].key = true
			}
			continue
		}
		if top >= 0 && stack[top].object && stack[top].key {
			key, _ = token.(string)
			stack[top].key = false
			continue
		}
		inValue := top >= 0 && stack[top].object
		switch v := token.(type) {
		case json.Delim:
			stack = append(stack, frame{object: v == '{', key: v == '{'})
			continue
		case json.Number:
			if !strings.ContainsAny(string(v), ".eE") {
				if _, err := strconv.ParseInt(string(v), 10, 64); err != nil {
					return database.Fail(contracts.InvalidArgument, `integer exceeds the 64-bit range; use {"$numberDecimal": "..."}`, false)
				}
			}
		case string:
			if inValue && key == "$numberDecimal" && len(v) > maxDecimalText {
				return database.Fail(contracts.InvalidArgument, "MongoDB numeric value is too long", false)
			}
		}
		if inValue {
			stack[top].key = true
		}
	}
}

// checkScript rejects server-side JavaScript anywhere in a document: BSON code
// values, the $where, $function and $accumulator operators, and every internal
// $_ operator, some of which evaluate JavaScript strings.
func checkScript(doc bson.Raw) error {
	return each(doc, func(key string, v bson.RawValue) error {
		switch {
		case v.Type == bson.TypeJavaScript, v.Type == bson.TypeCodeWithScope,
			key == "$where", key == "$function", key == "$accumulator", strings.HasPrefix(key, "$_"):
			return scriptRejected()
		case v.Type == bson.TypeEmbeddedDocument, v.Type == bson.TypeArray:
			return checkScript(bson.Raw(v.Value))
		}
		return nil
	})
}

// allowedStage reports whether an aggregation stage reads only. The writing
// $out and $merge stages and database-level, tailable, unreviewed, internal
// and unknown stages are not allowed.
func allowedStage(name string) bool {
	switch name {
	case "$addFields", "$set", "$unset", "$project", "$replaceRoot", "$replaceWith", "$match", "$limit", "$skip",
		"$sort", "$sortByCount", "$count", "$group", "$bucket", "$bucketAuto", "$unwind", "$sample", "$redact",
		"$setWindowFields", "$densify", "$fill", "$graphLookup",
		"$lookup", "$unionWith", "$facet",
		// The server enforces where these first-stage readers may appear.
		"$collStats", "$indexStats", "$planCacheStats", "$geoNear", "$search", "$searchMeta", "$vectorSearch",
		"$listSearchIndexes", "$listSessions", "$documents":
		return true
	}
	return false
}

// checkPipeline applies the stage allowlist to a pipeline and to every nested
// $lookup, $unionWith and $facet pipeline. Positional rules are the server's.
func checkPipeline(stages []bson.Raw) error {
	for _, stage := range stages {
		m, err := fields(stage)
		if err != nil || len(m) != 1 {
			return invalidStage()
		}
		for name, v := range m {
			if !strings.HasPrefix(name, "$") {
				return invalidStage()
			}
			if name == "$out" || name == "$merge" {
				return database.Fail(contracts.ReadOnlyViolation, "aggregation pipelines cannot write with $out or $merge", false)
			}
			if !allowedStage(name) {
				return database.Fail(contracts.QueryUnsupported, "aggregation stage is not supported", false)
			}
			if err := checkNested(name, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkNested(name string, v bson.RawValue) error {
	switch name {
	case "$lookup", "$unionWith":
		if name == "$unionWith" && v.Type == bson.TypeString {
			return nil
		}
		doc, ok := v.DocumentOK()
		if !ok {
			return invalidSubPipeline()
		}
		m, err := fields(doc)
		if err != nil {
			return invalidSubPipeline()
		}
		if pipeline, ok := m["pipeline"]; ok {
			return checkSubPipeline(pipeline)
		}
	case "$facet":
		doc, ok := v.DocumentOK()
		if !ok {
			return invalidSubPipeline()
		}
		return each(doc, func(_ string, facet bson.RawValue) error {
			return checkSubPipeline(facet)
		})
	}
	return nil
}

func checkSubPipeline(v bson.RawValue) error {
	list, err := values(v, math.MaxInt)
	if err != nil {
		return invalidSubPipeline()
	}
	stages := make([]bson.Raw, 0, len(list))
	for _, item := range list {
		stage, ok := item.DocumentOK()
		if !ok {
			return invalidSubPipeline()
		}
		stages = append(stages, stage)
	}
	return checkPipeline(stages)
}

// batchSize is the documents requested per batch: one more than the row limit
// detects truncation without decoding the extra document. It is absent when
// that count does not fit.
func batchSize(rowLimit int) (int64, bool) {
	if int64(rowLimit) == math.MaxInt64 {
		return 0, false
	}
	return int64(rowLimit) + 1, true
}

// findCommand builds a find from parsed inputs. Nothing but these arguments is
// ever sent: no hint, collation, let, comment, read concern or session.
func findCommand(collection string, filter, projection, sort bson.Raw, skip, rowLimit int) bson.D {
	if filter == nil {
		filter = emptyDocument()
	}
	cmd := bson.D{kv("find", collection), kv("filter", filter)}
	if projection != nil {
		cmd = append(cmd, kv("projection", projection))
	}
	if sort != nil {
		cmd = append(cmd, kv("sort", sort))
	}
	if skip > 0 {
		cmd = append(cmd, kv("skip", int64(skip)))
	}
	if n, ok := batchSize(rowLimit); ok {
		cmd = append(cmd, kv("limit", n), kv("batchSize", n))
	}
	return cmd
}

// aggregateCommand builds an aggregate over checked stages; no $limit is appended.
func aggregateCommand(collection string, stages []bson.Raw, rowLimit int) bson.D {
	pipeline := make(bson.A, 0, len(stages))
	for _, stage := range stages {
		pipeline = append(pipeline, stage)
	}
	cursor := bson.D{}
	if n, ok := batchSize(rowLimit); ok {
		cursor = bson.D{kv("batchSize", n)}
	}
	return bson.D{kv("aggregate", collection), kv("pipeline", pipeline), kv("cursor", cursor)}
}

func emptyDocument() bson.Raw { return bson.Raw{5, 0, 0, 0, 0} }
