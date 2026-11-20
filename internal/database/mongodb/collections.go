package mongodb

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const defaultPageSize = 100

func badCatalog() error {
	return database.Fail(contracts.QueryFailed, "invalid MongoDB catalog response", false)
}

func serverCode(err error, codes ...int32) bool {
	var server serverError
	return errors.As(err, &server) && slices.Contains(codes, server.code)
}

// collectionKind maps listCollections types to the public vocabulary.
func collectionKind(kind string) string {
	switch kind {
	case "collection", "view", "timeseries":
		return kind
	}
	return "other"
}

func pageSize(requested int) (int, error) {
	if requested == 0 {
		return defaultPageSize, nil
	}
	if requested < 1 || requested == math.MaxInt {
		return 0, database.Fail(contracts.InvalidArgument, "invalid metadata page request", false)
	}
	return requested, nil
}

type collectionEntry struct{ name, kind string }

// ListCollections returns a keyset page of the collections, views and
// time-series collections the account may list, ordered by database and then
// collection name, byte-wise. Each page lists only what it needs from each
// database, so memory stays proportional to the page.
func (d *Driver) ListCollections(ctx context.Context, a database.Access, req database.CollectionPageRequest) (database.CollectionPage, error) {
	a, rev, err := normalizeAccess(a)
	if err != nil {
		return database.CollectionPage{}, err
	}
	size, err := pageSize(req.PageSize)
	if err != nil {
		return database.CollectionPage{}, err
	}
	if req.Database != "" && config.MongoDBDatabaseName(req.Database) != nil {
		return database.CollectionPage{}, database.Fail(contracts.InvalidArgument, "invalid metadata page request", false)
	}
	// The position's Schema is a database and its Name a collection.
	pos := pool.Cursor{Version: 1, Tool: "list_collections", ID: a.Profile.ID, Revision: rev, Filter: req.Database, Epoch: d.pools.Epoch()}
	if req.Cursor != "" {
		if pos, err = d.pools.Decode(req.Cursor, pos, validName); err != nil {
			return database.CollectionPage{}, err
		}
	}
	out := database.CollectionPage{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Collections: []database.CollectionInfo{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		dbs := []string{req.Database}
		if req.Database == "" {
			var err error
			if dbs, err = databases(ctx, s, a); err != nil {
				return err
			}
		}
		need := size + 1
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		// If even the smallest possible objects cannot fill the requested
		// page within the cap, every observed entry must fit. More entries
		// could only overflow the page after it already exceeded the cap.
		const minEntryBytes = len(`{"database":"","name":"","type":""}`)
		chargeWhileListing := budget.Limit > 0 && size > budget.Limit/minEntryBytes
		var found []database.CollectionInfo
		for _, db := range dbs {
			if db < pos.Schema {
				continue
			}
			after := ""
			if db == pos.Schema {
				after = pos.Name
			}
			var accept func(collectionEntry) (bool, error)
			if chargeWhileListing {
				accept = func(e collectionEntry) (bool, error) {
					return true, budget.Add(database.CollectionInfo{Database: db, Name: e.name, Type: e.kind})
				}
			}
			entries, err := collections(ctx, s, a, db, after, need-len(found), accept)
			if err != nil {
				return err
			}
			for _, e := range entries {
				found = append(found, database.CollectionInfo{Database: db, Name: e.name, Type: e.kind})
			}
			if len(found) >= need {
				break
			}
		}
		if len(found) > 0 {
			out.Collections = found[:min(size, len(found))]
		}
		if !chargeWhileListing && budget.Limit > 0 {
			for _, item := range out.Collections {
				if err := budget.Add(item); err != nil {
					return err
				}
			}
		}
		if len(found) > size {
			last := found[size-1]
			pos.Schema, pos.Name = last.Database, last.Name
			token := d.pools.Encode(pos)
			if len(token) > pool.MaxCursorLength {
				return database.Fail(contracts.ResourceLimit, "continuation cursor exceeds its size limit; use a different page_size or database filter", false)
			}
			out.NextCursor = &token
		}
		return database.PayloadBound(out, a)
	})
	if err != nil {
		return database.CollectionPage{}, err
	}
	return out, nil
}

// databases lists the databases the account is authorized for, byte-ordered
// and without duplicates.
func databases(ctx context.Context, s *session, a database.Access) ([]string, error) {
	body, err := s.result(ctx, "admin", withMaxTime(ctx, a, bson.D{kv("listDatabases", int32(1)), kv("nameOnly", true), kv("authorizedDatabases", true)}))
	if err != nil {
		return nil, err
	}
	top, err := fields(body)
	if err != nil {
		return nil, badCatalog()
	}
	list, err := values(top["databases"], math.MaxInt)
	if err != nil {
		return nil, badCatalog()
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		doc, ok := item.DocumentOK()
		if !ok {
			return nil, badCatalog()
		}
		m, err := fields(doc)
		if err != nil {
			return nil, badCatalog()
		}
		name, ok := m["name"].StringValueOK()
		if !ok || name == "" {
			return nil, badCatalog()
		}
		if err = checkResultName(name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// collections lists collection names in db after the given name, keeping the
// keep byte-smallest when keep is positive. accept can skip an entry or stop
// the listing before the entry is retained. An account without access to the
// database sees no collections.
func collections(ctx context.Context, s *session, a database.Access, db, after string, keep int, accept func(collectionEntry) (bool, error)) (out []collectionEntry, err error) {
	cmd := bson.D{kv("listCollections", int32(1)), kv("nameOnly", true), kv("authorizedCollections", true)}
	if after != "" {
		cmd = append(cmd, kv("filter", bson.D{kv("name", bson.D{kv("$gt", after)})}))
	}
	cmd = append(cmd, kv("cursor", bson.D{}))
	cur, err := s.openCursor(ctx, a, db, withMaxTime(ctx, a, cmd))
	if serverCode(err, codeUnauthorized) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := cur.close(); e != nil && err == nil {
			out, err = nil, e
		}
	}()
	byName := func(x, y collectionEntry) int { return strings.Compare(x.name, y.name) }
	for {
		doc, more, err := cur.next(ctx, 0)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		m, err := fields(doc)
		if err != nil {
			return nil, badCatalog()
		}
		name, ok := m["name"].StringValueOK()
		if !ok || name == "" {
			return nil, badCatalog()
		}
		if err = checkResultName(name); err != nil {
			return nil, err
		}
		// The server filter is rechecked so a page can never repeat an entry.
		if name <= after {
			continue
		}
		kind, _ := m["type"].StringValueOK()
		entry := collectionEntry{name: name, kind: collectionKind(kind)}
		if accept != nil {
			include, err := accept(entry)
			if err != nil {
				return nil, err
			}
			if !include {
				continue
			}
		}
		out = append(out, entry)
		if keep > 0 && len(out)/2 >= keep {
			slices.SortFunc(out, byName)
			out = out[:keep]
		}
	}
	slices.SortFunc(out, byName)
	if keep > 0 && len(out) > keep {
		out = out[:keep]
	}
	return out, nil
}

// DescribeCollection reports a collection's type, creation options and index
// specifications without reading documents. A missing or inaccessible target
// is PERMISSION_DENIED, as for other metadata.
func (d *Driver) DescribeCollection(ctx context.Context, a database.Access, req database.CollectionRequest) (database.CollectionDescription, error) {
	a, rev, err := normalizeAccess(a)
	if err != nil {
		return database.CollectionDescription{}, err
	}
	if err = checkNames(req.Database, req.Collection); err != nil {
		return database.CollectionDescription{}, err
	}
	out := database.CollectionDescription{Connection: a.Profile.Alias, Driver: a.Profile.Driver, Database: req.Database, Collection: req.Collection, Indexes: []json.RawMessage{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		kind, options, err := collectionOptions(ctx, s, a, req.Database, req.Collection)
		if err != nil {
			return err
		}
		if err := budget.Add(options); err != nil {
			return err
		}
		out.Type, out.Options = kind, options
		if kind == "view" {
			return database.PayloadBound(out, a)
		}
		err = indexSpecs(ctx, s, a, req.Database, req.Collection, func(_ string, spec bson.Raw) error {
			encoded, err := encodeDocument(spec)
			if err != nil {
				return err
			}
			if err := budget.Add(encoded); err != nil {
				return err
			}
			out.Indexes = append(out.Indexes, encoded)
			return nil
		})
		if serverCode(err, codeUnauthorized, codeNamespaceNotFound, codeCommandNotSupportedOnView) {
			return inaccessible()
		}
		if err != nil {
			return err
		}
		return database.PayloadBound(out, a)
	})
	if err != nil {
		return database.CollectionDescription{}, err
	}
	return out, nil
}

// collectionOptions reads one listCollections entry by exact name.
func collectionOptions(ctx context.Context, s *session, a database.Access, db, coll string) (kind string, options json.RawMessage, err error) {
	cmd := bson.D{kv("listCollections", int32(1)), kv("filter", bson.D{kv("name", coll)}), kv("cursor", bson.D{})}
	cur, err := s.openCursor(ctx, a, db, withMaxTime(ctx, a, cmd))
	if serverCode(err, codeUnauthorized, codeNamespaceNotFound) {
		return "", nil, inaccessible()
	}
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if e := cur.close(); e != nil && err == nil {
			kind, options, err = "", nil, e
		}
	}()
	var entry bson.Raw
	for {
		doc, more, err := cur.next(ctx, 0)
		if err != nil {
			return "", nil, err
		}
		if !more {
			break
		}
		if entry != nil {
			return "", nil, badCatalog()
		}
		entry = doc
	}
	if entry == nil {
		return "", nil, inaccessible()
	}
	m, err := fields(entry)
	if err != nil {
		return "", nil, badCatalog()
	}
	if name, ok := m["name"].StringValueOK(); !ok || name != coll {
		return "", nil, badCatalog()
	}
	kind, _ = m["type"].StringValueOK()
	options = json.RawMessage(`{}`)
	if v, ok := m["options"]; ok {
		doc, ok := v.DocumentOK()
		if !ok {
			return "", nil, badCatalog()
		}
		if options, err = encodeDocument(doc); err != nil {
			return "", nil, err
		}
	}
	return collectionKind(kind), options, nil
}

// indexSpecs visits each index specification in server order after checking
// that it has a string name and a key document. Server failures are returned
// unclassified so callers can treat an inaccessible target as they need.
func indexSpecs(ctx context.Context, s *session, a database.Access, db, coll string, visit func(name string, spec bson.Raw) error) (err error) {
	cur, err := s.openCursor(ctx, a, db, withMaxTime(ctx, a, bson.D{kv("listIndexes", coll), kv("cursor", bson.D{})}))
	if err != nil {
		return err
	}
	defer func() {
		if e := cur.close(); e != nil && err == nil {
			err = e
		}
	}()
	for {
		spec, more, err := cur.next(ctx, 0)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		m, err := fields(spec)
		if err != nil {
			return badCatalog()
		}
		name, ok := m["name"].StringValueOK()
		if _, okKey := m["key"].DocumentOK(); !ok || name == "" || !okKey {
			return badCatalog()
		}
		if err = checkResultName(name); err != nil {
			return err
		}
		if err := visit(name, spec); err != nil {
			return err
		}
	}
}

// DescribeDatabase lists authorized databases as schemas, except admin, local
// and config. Collections, views and time-series collections are tables,
// excluding system collections, and index names carry their collection. No
// document is read and the profile's authentication database is not a scope.
func (d *Driver) DescribeDatabase(ctx context.Context, a database.Access) (database.DatabaseDescription, error) {
	a, rev, err := normalizeAccess(a)
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	out := database.DatabaseDescription{Version: 1, Alias: a.Profile.Alias, Driver: a.Profile.Driver, Schemas: []database.SchemaDescription{}}
	err = d.runNormalized(ctx, a, rev, func(ctx context.Context, s *session) error {
		names, err := databases(ctx, s, a)
		if err != nil {
			return err
		}
		budget := database.MetadataBudget{Limit: a.Profile.Limits.MaxResultBytes}
		for _, db := range names {
			switch db {
			case "admin", "local", "config":
				continue
			}
			if err := describeDatabase(ctx, s, a, db, &out, &budget); err != nil {
				return err
			}
		}
		return out.CheckSize(budget.Limit)
	})
	if err != nil {
		return database.DatabaseDescription{}, err
	}
	return out, nil
}

func describeDatabase(ctx context.Context, s *session, a database.Access, db string, out *database.DatabaseDescription, budget *database.MetadataBudget) error {
	// Charge entries while listing so a huge catalog fails before it is held.
	listing := database.MetadataBudget{Limit: budget.Limit}
	entries, err := collections(ctx, s, a, db, "", 0, func(e collectionEntry) (bool, error) {
		if strings.HasPrefix(e.name, "system.") {
			return false, nil
		}
		return true, listing.Add(database.RelationName{Name: e.name, Kind: e.kind})
	})
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return out.AddCatalogRow(budget, db, "", "", "")
	}
	for _, e := range entries {
		if err := out.AddCatalogRow(budget, db, e.kind, e.name, ""); err != nil {
			return err
		}
	}
	var indexes []database.IndexName
	for _, e := range entries {
		if e.kind == "view" {
			continue
		}
		var names []database.IndexName
		err := indexSpecs(ctx, s, a, db, e.name, func(name string, _ bson.Raw) error {
			names = append(names, database.IndexName{Name: name, Table: e.name})
			return listing.Add(names[len(names)-1])
		})
		if serverCode(err, codeUnauthorized, codeNamespaceNotFound, codeCommandNotSupportedOnView) {
			continue
		}
		if err != nil {
			return err
		}
		indexes = append(indexes, names...)
	}
	slices.SortFunc(indexes, func(x, y database.IndexName) int {
		return cmp.Or(strings.Compare(x.Name, y.Name), strings.Compare(x.Table, y.Table))
	})
	for _, index := range indexes {
		if err := out.AddCatalogRow(budget, db, "index", index.Name, index.Table); err != nil {
			return err
		}
	}
	return nil
}
