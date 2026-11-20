package mongodb

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// rowLimit applies the query model: zero means the profile's max_rows, which
// a request may lower but never raise.
func rowLimit(requested int, a database.Access) (int, error) {
	limit := a.Profile.Limits.MaxRows
	if requested < 0 || requested > limit {
		return 0, database.Fail(contracts.InvalidArgument, "row limit exceeds profile bounds", false)
	}
	if requested != 0 {
		limit = requested
	}
	return limit, nil
}

// FindDocuments runs one find. Every input is parsed and guarded before
// admission; complete documents return as canonical Extended JSON up to the
// row and byte limits, without a continuation.
func (d *Driver) FindDocuments(ctx context.Context, a database.Access, req database.FindRequest) (database.DocumentResult, error) {
	started := time.Now()
	a, rev, err := normalizeAccess(a)
	if err != nil {
		return database.DocumentResult{}, err
	}
	limit, err := rowLimit(req.RowLimit, a)
	if err != nil {
		return database.DocumentResult{}, err
	}
	if req.Skip < 0 {
		return database.DocumentResult{}, database.Fail(contracts.InvalidArgument, "skip must not be negative", false)
	}
	if err = checkNames(req.Database, req.Collection); err != nil {
		return database.DocumentResult{}, err
	}
	var parts [3]bson.Raw
	for i, raw := range []json.RawMessage{req.Filter, req.Projection, req.Sort} {
		if parts[i], err = parseDocument(raw, true); err != nil {
			return database.DocumentResult{}, err
		}
	}
	for _, part := range parts {
		if part == nil {
			continue
		}
		if err = checkScript(part); err != nil {
			return database.DocumentResult{}, err
		}
	}
	cmd := findCommand(req.Collection, parts[0], parts[1], parts[2], req.Skip, limit)
	return d.queryDocuments(ctx, a, rev, req.Database, cmd, limit, started)
}

// AggregateDocuments runs one pipeline of read stages, including nested
// $lookup, $unionWith and $facet pipelines, under the find result model.
func (d *Driver) AggregateDocuments(ctx context.Context, a database.Access, req database.AggregateRequest) (database.DocumentResult, error) {
	started := time.Now()
	a, rev, err := normalizeAccess(a)
	if err != nil {
		return database.DocumentResult{}, err
	}
	limit, err := rowLimit(req.RowLimit, a)
	if err != nil {
		return database.DocumentResult{}, err
	}
	if err = checkNames(req.Database, req.Collection); err != nil {
		return database.DocumentResult{}, err
	}
	stages := make([]bson.Raw, len(req.Pipeline))
	for i, raw := range req.Pipeline {
		if stages[i], err = parseDocument(raw, false); err != nil {
			return database.DocumentResult{}, err
		}
	}
	for _, stage := range stages {
		if err = checkScript(stage); err != nil {
			return database.DocumentResult{}, err
		}
	}
	if err = checkPipeline(stages); err != nil {
		return database.DocumentResult{}, err
	}
	cmd := aggregateCommand(req.Collection, stages, limit)
	return d.queryDocuments(ctx, a, rev, req.Database, cmd, limit, started)
}

// queryDocuments runs a built read command under admission and a pooled session.
func (d *Driver) queryDocuments(ctx context.Context, a database.Access, rev config.Revision, db string, cmd bson.D, limit int, started time.Time) (database.DocumentResult, error) {
	var out database.DocumentResult
	err := d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		var e error
		out, e = readDocuments(ctx, s, a, db, cmd, limit, started)
		return e
	})
	if err != nil {
		return database.DocumentResult{}, err
	}
	return out, nil
}

// readDocuments opens the cursor and stops at the row limit or the byte budget.
// One extra document detects truncation and is never decoded. The cursor is
// killed before the result is returned.
func readDocuments(ctx context.Context, s *session, a database.Access, db string, cmd bson.D, limit int, started time.Time) (out database.DocumentResult, err error) {
	b, err := newDocumentBuilder(a, limit, started)
	if err != nil {
		return database.DocumentResult{}, err
	}
	cur, err := s.openCursor(ctx, a, db, withMaxTime(ctx, a, cmd))
	if err != nil {
		return database.DocumentResult{}, err
	}
	defer func() {
		if e := cur.close(); e != nil && err == nil {
			out, err = database.DocumentResult{}, e
		}
	}()
	want, bounded := batchSize(limit)
	for {
		// Every document read so far was added, so the remaining batch is
		// what the row limit and the truncation probe still need.
		size := int64(0)
		if bounded {
			size = want - int64(len(b.out.Documents))
		}
		doc, more, err := cur.next(ctx, size)
		if err != nil {
			return database.DocumentResult{}, err
		}
		if !more || !b.next() {
			break
		}
		encoded, err := encodeDocument(doc)
		if err != nil {
			return database.DocumentResult{}, err
		}
		if !b.add(encoded) {
			break
		}
	}
	return b.result(), nil
}

// documentBuilder accounts a document envelope as documents arrive, exactly
// as database.ResultBuilder accounts query rows.
type documentBuilder struct {
	out      database.DocumentResult
	budget   database.ArrayBudget
	rowLimit int
	started  time.Time
}

// newDocumentBuilder reserves the envelope header at maximum field widths, so
// each appended document is counted exactly once.
func newDocumentBuilder(a database.Access, rowLimit int, started time.Time) (*documentBuilder, error) {
	out := database.DocumentResult{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Documents: []json.RawMessage{}}
	header := out
	header.DocumentCount, header.ElapsedMS = rowLimit, math.MaxInt64
	budget, err := database.NewArrayBudget(a.Profile.Limits.MaxResultBytes, header, "result envelope exceeds result budget")
	if err != nil {
		return nil, err
	}
	return &documentBuilder{out: out, budget: budget, rowLimit: rowLimit, started: started}, nil
}

// next reports whether another document may be read; at the row limit the
// result is truncated.
func (b *documentBuilder) next() bool {
	if len(b.out.Documents) == b.rowLimit {
		b.out.Truncated = true
		return false
	}
	return true
}

// add appends one document, already in the compact JSON form the MCP envelope
// carries, unless it would exceed the byte cap, in which case the result is
// truncated and the document dropped.
func (b *documentBuilder) add(doc json.RawMessage) bool {
	if !b.budget.Charge(doc, len(b.out.Documents)) {
		b.out.Truncated = true
		return false
	}
	b.out.Documents = append(b.out.Documents, doc)
	return true
}

// result finalizes counts and elapsed time.
func (b *documentBuilder) result() database.DocumentResult {
	b.out.DocumentCount = len(b.out.Documents)
	b.out.ElapsedMS = time.Since(b.started).Milliseconds()
	return b.out
}
