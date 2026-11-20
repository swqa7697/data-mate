package mongodb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Server cursors never outlive the operation that opened them: a truncated
// read kills its cursor before the connection returns to the pool, a failed
// kill cannot report success or keep the connection, and a cursor whose
// connection cancellation closed is killed from a short side connection.
// Real servers cannot be made to fail killCursors or to hold a reply.
func TestServerCursorCleanup(t *testing.T) {
	credentials := scramCredentials(t)
	limits := config.Limits{QueryTimeoutMS: 5000, MaxRows: 50000}
	item := func(id int32) bson.D { return bson.D{kv("_id", id), kv("name", "item")} }
	batch := func(key string, doc bson.D) bson.D {
		return ok(kv("cursor", bson.D{kv(key, bson.A{doc}), kv("id", int64(77)), kv("ns", "app.items")}))
	}
	expect := func(req bson.Raw, key string, want any) error {
		kind, value, err := bson.MarshalValue(want)
		got, lookup := req.LookupErr(key)
		if err != nil || lookup != nil || got.Type != kind || !bytes.Equal(got.Value, value) {
			return fmt.Errorf("%s is not %v", key, want)
		}
		return nil
	}
	// row_limit 1 asks for one extra document to detect truncation.
	find := exchange{command: "find", reply: send(batch("firstBatch", item(1))), check: func(req bson.Raw) error {
		for key, want := range map[string]any{"find": "items", "filter": bson.D{}, "limit": int64(2), "batchSize": int64(2), "$db": "app"} {
			if err := expect(req, key, want); err != nil {
				return err
			}
		}
		if ms, ok := req.Lookup("maxTimeMS").Int64OK(); !ok || ms <= 0 || ms > 5000 {
			return errors.New("find lacks the remaining profile time budget")
		}
		return nil
	}}
	getMore := func(reply func(bson.Raw, int32) []byte) exchange {
		return exchange{command: "getMore", reply: reply, check: func(req bson.Raw) error {
			for key, want := range map[string]any{"getMore": int64(77), "collection": "items", "batchSize": int64(1)} {
				if err := expect(req, key, want); err != nil {
					return err
				}
			}
			if _, err := req.LookupErr("maxTimeMS"); err == nil {
				return errors.New("getMore carries maxTimeMS")
			}
			return nil
		}}
	}
	kill := func(reply bson.D) exchange {
		return exchange{command: "killCursors", reply: send(reply), check: func(req bson.Raw) error {
			if err := expect(req, "killCursors", "items"); err != nil {
				return err
			}
			return expect(req, "cursors", bson.A{int64(77)})
		}}
	}
	killed := ok(kv("cursorsKilled", bson.A{int64(77)}), kv("cursorsNotFound", bson.A{}), kv("cursorsAlive", bson.A{}), kv("cursorsUnknown", bson.A{}))
	// run reads with row_limit 1, records the pool, then closes it so the
	// peer sees every connection end.
	run := func(t *testing.T, scripts ...[]exchange) (database.DocumentResult, pool.Stats, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		peer := newPeer(t, cancel, false, scripts...)
		r := fakeRegistry(t)
		p := fakeProfile(peer.port(), limits)
		out, err := New(r).FindDocuments(ctx, database.NewAccess(p, fakeSecret), database.FindRequest{Database: "app", Collection: "items", RowLimit: 1})
		stats := r.Stats(p.ID)
		r.Close()
		peer.wait(t)
		return out, stats, err
	}

	t.Run("truncation kills the cursor before the connection is reused", func(t *testing.T) {
		out, stats, err := run(t, ready(newScramPeer(credentials), find, getMore(send(batch("nextBatch", item(2)))), kill(killed)))
		if err != nil || !out.Truncated || out.DocumentCount != 1 || len(out.Documents) != 1 || string(out.Documents[0]) != `{"_id":{"$numberInt":"1"},"name":"item"}` {
			t.Fatalf("unexpected result %+v, error %v", out, err)
		}
		if stats.Idle != 1 {
			t.Fatalf("%d idle connections after a clean kill, want 1", stats.Idle)
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if err := contracts.Validate("find_documents.output", b); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("failed kill discards the connection", func(t *testing.T) {
		_, stats, err := run(t, ready(newScramPeer(credentials), find, getMore(send(batch("nextBatch", item(2)))), kill(failure(8000))))
		var safe *database.Error
		if !errors.As(err, &safe) || safe.Code != contracts.ConnectFailed {
			t.Fatalf("cleanup failure returned %v", err)
		}
		if stats.Idle != 0 || stats.Live != 0 {
			t.Fatalf("%d idle and %d live connections after a failed kill", stats.Idle, stats.Live)
		}
	})

	t.Run("cancelled getMore kills the cursor from a side connection", func(t *testing.T) {
		_, _, err := run(t,
			ready(newScramPeer(credentials), find, getMore(nil)),
			handshake(newScramPeer(credentials), kill(killed)))
		var safe *database.Error
		if !errors.As(err, &safe) || safe.Code != contracts.Cancelled {
			t.Fatalf("cancelled read returned %v", err)
		}
	})

	// A peer can drop the connection after the last reply, so a read that
	// otherwise succeeded must fail when the side kill is unconfirmed.
	t.Run("unconfirmed side kill cannot report success", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		raw, peer := net.Pipe()
		defer peer.Close()
		s := newSession(raw, raw)
		s.Close()
		c := &serverCursor{s: s, a: database.NewAccess(fakeProfile(port, limits), fakeSecret), db: "app", coll: "items", id: 77}
		var safe *database.Error
		if err := c.close(); !errors.As(err, &safe) || safe.Code != contracts.ConnectFailed {
			t.Fatalf("unconfirmed kill returned %v", err)
		}
	})
}
