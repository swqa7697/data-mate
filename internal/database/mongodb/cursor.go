package mongodb

import (
	"context"
	"strings"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// maxEmptyBatches bounds consecutive getMore replies that return no documents.
const maxEmptyBatches = 64

func badCursor() error {
	return database.Fail(contracts.QueryFailed, "invalid MongoDB cursor response", false)
}

// serverCursor is a server cursor that never outlives the Work call that
// opened it: every caller defers close.
type serverCursor struct {
	s     *session
	a     database.Access
	db    string
	coll  string
	id    int64
	batch []bson.Raw
	empty int
}

// openCursor sends a cursor-returning command and reads its first batch.
func (s *session) openCursor(ctx context.Context, a database.Access, db string, cmd bson.D) (*serverCursor, error) {
	body, err := s.result(ctx, db, cmd)
	if err != nil {
		return nil, err
	}
	id, coll, batch, err := parseCursor(body, db, true)
	if err != nil {
		s.Close()
		return nil, err
	}
	return &serverCursor{s: s, a: a, db: db, coll: coll, id: id, batch: batch}, nil
}

// next returns the next document, fetching at most batchSize more with
// getMore when the current batch is exhausted; zero leaves the size to the
// server. getMore never carries maxTimeMS; the cursor's command already did.
func (c *serverCursor) next(ctx context.Context, batchSize int64) (bson.Raw, bool, error) {
	for len(c.batch) == 0 {
		if c.id == 0 {
			return nil, false, nil
		}
		cmd := bson.D{kv("getMore", c.id), kv("collection", c.coll)}
		if batchSize > 0 {
			cmd = append(cmd, kv("batchSize", batchSize))
		}
		body, err := c.s.result(ctx, c.db, cmd)
		if err != nil {
			return nil, false, err
		}
		id, _, batch, err := parseCursor(body, c.db, false)
		if err != nil || (id != c.id && id != 0) {
			c.s.Close()
			return nil, false, badCursor()
		}
		c.id, c.batch = id, batch
		switch {
		case len(batch) > 0:
			c.empty = 0
		case id != 0:
			if c.empty++; c.empty > maxEmptyBatches {
				return nil, false, database.Fail(contracts.QueryFailed, "MongoDB cursor returned too many empty batches", false)
			}
		}
	}
	doc := c.batch[0]
	c.batch = c.batch[1:]
	return doc, true, nil
}

// close kills a live server cursor. On an open session, killCursors runs on
// the same connection within the cleanup budget, even after cancellation; if
// it fails, the session is discarded. When cancellation or a route failure
// already closed the session, a side connection kills the known cursor.
// Either way, an unconfirmed kill is an error, so the operation cannot report
// success while its cursor may survive; callers keep their own earlier error.
func (c *serverCursor) close() error {
	if c.id == 0 {
		return nil
	}
	id := c.id
	c.id, c.batch = 0, nil
	if !c.s.Reusable() {
		if killOrphan(c.a, c.db, c.coll, id) {
			return nil
		}
		return cleanupFailed()
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	body, err := c.s.control(ctx, c.db, killCursors(c.coll, id))
	if err == nil && killed(body, id, false) {
		return nil
	}
	c.s.Close()
	return cleanupFailed()
}

func cleanupFailed() error {
	return database.Fail(contracts.ConnectFailed, "MongoDB cursor cleanup failed", true)
}

func killCursors(coll string, id int64) bson.D {
	return bson.D{kv("killCursors", coll), kv("cursors", bson.A{id})}
}

// killed reports that the server no longer holds the cursor: it was killed or
// had already ended, and is not alive. On the cursor's own connection it must
// also not be unknown; a side connection may find that the server already
// reaped it, so there unknown means gone.
func killed(body bson.Raw, id int64, unknownGone bool) bool {
	top, err := fields(body)
	if err != nil {
		return false
	}
	keys := []string{"cursorsAlive", "cursorsUnknown"}
	if unknownGone {
		keys = keys[:1]
	}
	for _, key := range keys {
		v, ok := top[key]
		if !ok {
			continue
		}
		// One cursor was named, so each list holds at most that cursor.
		list, err := values(v, 1)
		if err != nil {
			return false
		}
		for _, item := range list {
			if n, ok := item.Int64OK(); !ok || n == id {
				return false
			}
		}
	}
	return true
}

// killOrphan stops a known cursor after its session closed and reports
// whether the server confirmed it is gone. It is bounded: a short-lived
// connection to the same endpoint, route and account, without an account
// audit, sends killCursors and is closed.
func killOrphan(a database.Access, db, coll string, id int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	s, err := connect(ctx, a, nil)
	if err != nil {
		return false
	}
	defer s.Close()
	body, err := s.control(ctx, db, killCursors(coll, id))
	return err == nil && killed(body, id, true)
}

// parseCursor reads a cursor envelope: an int64 id, a namespace in db and a
// batch of documents only. The namespace's remainder is the collection that
// getMore and killCursors must name, which differs from the command's target
// for catalog cursors.
func parseCursor(body bson.Raw, db string, first bool) (int64, string, []bson.Raw, error) {
	top, err := fields(body)
	if err != nil {
		return 0, "", nil, badCursor()
	}
	envelope, ok := top["cursor"].DocumentOK()
	if !ok {
		return 0, "", nil, badCursor()
	}
	cursor, err := fields(envelope)
	if err != nil {
		return 0, "", nil, badCursor()
	}
	id, okID := cursor["id"].Int64OK()
	ns, okNS := cursor["ns"].StringValueOK()
	coll, okColl := strings.CutPrefix(ns, db+".")
	key := "nextBatch"
	if first {
		key = "firstBatch"
	}
	batch, okBatch := cursor[key]
	if !okID || !okNS || !okColl || coll == "" || !okBatch || batch.Type != bson.TypeArray {
		return 0, "", nil, badCursor()
	}
	var docs []bson.Raw
	err = each(batch.Value, func(_ string, v bson.RawValue) error {
		doc, ok := v.DocumentOK()
		if !ok {
			return errProtocol
		}
		docs = append(docs, doc)
		return nil
	})
	if err != nil {
		return 0, "", nil, badCursor()
	}
	return id, coll, docs, nil
}
