package mongodb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Audit bounds keep one bounded reply from turning into unbounded local work.
const (
	maxRoles      = 4096
	maxPrivileges = 16384
	maxActions    = 256
)

func admissionDenied() error {
	return database.Fail(contracts.ReadOnlyViolation, "MongoDB privileges do not establish a dedicated read-only account", false)
}

func unverifiable() error {
	return database.Fail(contracts.ReadOnlyViolation, "cannot verify MongoDB account privileges; use a dedicated read-only account", false)
}

func authorizationOff() error {
	return database.Fail(contracts.ReadOnlyViolation, "MongoDB server does not enforce authorization; enable access control", false)
}

func auditLimit() error {
	return database.Fail(contracts.ResourceLimit, "MongoDB privileges exceed audit limit", false)
}

// allowedAction lists the privilege actions a dedicated reader may hold. Like
// the MySQL grant allowlist, it is fail-closed: anyAction, internal, user and
// role inspection, query settings, killing other users' operations or cursors
// and every write or administrative action are denied.
//
// Tier A reads data and catalog metadata. Tier B is read-only monitoring.
// clusterMonitor holds only allowed actions from MongoDB 8.2, but it inherits
// read on local and config and find on every system.profile, so on a replica
// set it can read the oplog, which records every write to every database,
// including credential changes. That exposure is documented, not blocked.
func allowedAction(action string) bool {
	switch action {
	// Tier A: reads.
	case "find", "listCollections", "listIndexes", "listSearchIndexes", "listDatabases", "collStats", "dbStats", "dbHash",
		"killCursors", "changeStream", "planCacheRead", "performRawDataOperations":
		return true
	// Tier B: read-only monitoring.
	case "serverStatus", "top", "inprog", "listSessions", "connPoolStats", "netstat", "hostInfo", "getCmdLineOpts",
		"getParameter", "getLog", "getDefaultRWConcern", "replSetGetConfig", "replSetGetStatus", "getShardMap", "listShards",
		"shardingState", "shardedDataDistribution", "listClusterCatalog", "allCollectionStats", "indexStats", "useUUID",
		"getDatabaseVersion", "getShardVersion", "queryStatsRead", "queryStatsReadTransformed", "listSampledQueries",
		"listExtensions", "getTransitionToDedicatedConfigServerStatus", "shardDrainingStatus":
		return true
	}
	return false
}

// resource keeps what the credential rule needs from a parsed resource.
// collection is empty for every shape but a database and collection pattern.
type resource struct {
	any        bool
	collection string
}

// resourceShape accepts exactly the resource documents a server privilege can
// expand to: cluster, anyResource, a database and collection pattern, or a
// time-series buckets pattern with or without a database.
func resourceShape(doc bson.Raw) (resource, error) {
	m, err := fields(doc)
	if err != nil {
		return resource{}, err
	}
	truth := func(key string) bool {
		b, _ := m[key].BooleanOK()
		return b
	}
	_, hasDB := m["db"].StringValueOK()
	collection, hasCollection := m["collection"].StringValueOK()
	_, hasBuckets := m["system_buckets"].StringValueOK()
	switch {
	case len(m) == 1 && truth("cluster"):
		return resource{}, nil
	case len(m) == 1 && truth("anyResource"):
		return resource{any: true}, nil
	case len(m) == 2 && hasDB && hasCollection:
		return resource{collection: collection}, nil
	case len(m) == 2 && hasDB && hasBuckets, len(m) == 1 && hasBuckets:
		return resource{}, nil
	}
	return resource{}, errProtocol
}

// credentialResource rejects privileges that expose stored credentials: any
// privilege on a credential collection in any database, and data reads on
// every resource, which include those collections. read, readAnyDatabase and
// clusterMonitor hold neither.
func credentialResource(r resource, actions []string) bool {
	switch r.collection {
	case "system.users", "system.backup_users", "system.keys":
		return true
	}
	if r.any {
		for _, action := range actions {
			switch action {
			case "find", "changeStream", "performRawDataOperations":
				return true
			}
		}
	}
	return false
}

// auditAccount checks the authenticated user's expanded privileges, then
// proves that the server enforces authorization. Credentials are never
// requested and denials never include server text. Every resource is checked,
// not only the reachable databases, because writes can target any namespace.
func auditAccount(ctx context.Context, s *session, user, db string) error {
	reply, err := s.control(ctx, "admin", bson.D{kv("connectionStatus", int32(1)), kv("showPrivileges", true)})
	if err != nil {
		var server serverError
		if errors.As(err, &server) {
			return unverifiable()
		}
		return err
	}
	if err := auditPrivileges(reply, user, db); err != nil {
		return err
	}
	return probeAuthorization(ctx, s)
}

func auditPrivileges(reply bson.Raw, user, db string) error {
	top, err := fields(reply)
	if err != nil {
		return unverifiable()
	}
	authInfo, ok := top["authInfo"].DocumentOK()
	if !ok {
		return unverifiable()
	}
	info, err := fields(authInfo)
	if err != nil {
		return unverifiable()
	}
	for key, v := range info {
		switch key {
		case "authenticatedUsers", "authenticatedUserRoles", "authenticatedUserPrivileges":
		case "uuid":
			if v.Type != bson.TypeBinary {
				return unverifiable()
			}
		default:
			return unverifiable()
		}
	}
	if !singleUser(info["authenticatedUsers"], user, db) {
		return admissionDenied()
	}
	if v, ok := info["authenticatedUserRoles"]; ok {
		roles, err := bounded(v, maxRoles)
		if err != nil {
			return err
		}
		for _, role := range roles {
			doc, ok := role.DocumentOK()
			if !ok {
				return unverifiable()
			}
			m, err := fields(doc)
			_, okRole := m["role"].StringValueOK()
			_, okDB := m["db"].StringValueOK()
			if err != nil || len(m) != 2 || !okRole || !okDB {
				return unverifiable()
			}
		}
	}
	privileges, err := bounded(info["authenticatedUserPrivileges"], maxPrivileges)
	if err != nil {
		return err
	}
	for _, privilege := range privileges {
		if err := auditPrivilege(privilege); err != nil {
			return err
		}
	}
	return nil
}

// bounded reads an audit array, distinguishing an over-limit reply.
func bounded(v bson.RawValue, limit int) ([]bson.RawValue, error) {
	list, err := values(v, limit)
	if errors.Is(err, errCount) {
		return nil, auditLimit()
	}
	if err != nil {
		return nil, unverifiable()
	}
	return list, nil
}

func auditPrivilege(privilege bson.RawValue) error {
	body, ok := privilege.DocumentOK()
	if !ok {
		return unverifiable()
	}
	m, err := fields(body)
	if err != nil || len(m) != 2 {
		return unverifiable()
	}
	doc, ok := m["resource"].DocumentOK()
	if !ok {
		return unverifiable()
	}
	r, err := resourceShape(doc)
	if err != nil {
		return unverifiable()
	}
	list, err := bounded(m["actions"], maxActions)
	if err != nil {
		return err
	}
	actions := make([]string, 0, len(list))
	for _, item := range list {
		action, ok := item.StringValueOK()
		if !ok {
			return unverifiable()
		}
		if !allowedAction(action) {
			return admissionDenied()
		}
		actions = append(actions, action)
	}
	if credentialResource(r, actions) {
		return admissionDenied()
	}
	return nil
}

// probeAuthorization asks for a random nonexistent user, which an account
// without viewUser may not do. A server that answers is not enforcing
// authorization, so its reported privileges prove nothing.
func probeAuthorization(ctx context.Context, s *session) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return unverifiable()
	}
	_, err := s.control(ctx, "admin", bson.D{kv("usersInfo", bson.D{kv("user", "data-mate-probe-"+hex.EncodeToString(nonce[:])), kv("db", "admin")})})
	if err == nil {
		return authorizationOff()
	}
	var server serverError
	if errors.As(err, &server) {
		if server.code == codeUnauthorized {
			return nil
		}
		return unverifiable()
	}
	return err
}
