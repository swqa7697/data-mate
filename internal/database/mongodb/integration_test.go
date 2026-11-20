package mongodb_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/mongodb"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// fixture is the owned container of make test-integration DB_DRIVER=mongodb.
// Port 27017 serves plaintext and TLS with authorization; noauth is the
// authorization-disabled single-node replica set that admission must refuse.
type fixture struct {
	root, container, image string
	port, noauth           int
	password               string
}

const fixtureID = "12345678-1234-1234-1234-123456789abc"

// profile returns a fixture profile for user in the admin authentication
// database. Profiles that change limits or users for pooled operations take a
// distinct ID so they never retire another scenario's pool.
func (f *fixture) profile(user, id string) config.Profile {
	limits := config.DefaultLimits()
	return config.Profile{ID: id, Alias: "fixture", Driver: "mongodb",
		Connection: config.Connection{Host: "localhost", Port: f.port, Database: "admin", Username: user},
		Transport:  config.Transport{TLS: config.TLS{Mode: "disabled"}}, Limits: &limits}
}

func (f *fixture) access(p config.Profile) database.Access { return database.NewAccess(p, f.password) }

// mongosh runs an administrative script in the owned container only. The root
// password is read inside the container from the private fixture directory and
// the script arrives on stdin, so no secret reaches a process argument.
func (f *fixture) mongosh(t *testing.T, script string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "--env", "HOME=/tmp", "--env", "DO_NOT_TRACK=1", f.container, "sh", "-ec",
		`f=$(mktemp --suffix=.js); cat >"$f"; exec mongosh --quiet --norc mongodb://127.0.0.1:27017/admin "$f"`)
	cmd.Stdin = strings.NewReader(`db.auth("root", require("fs").readFileSync("/fixture/admin-password", "utf8").trim());` + "\n" +
		script + "\nprint(\"data-mate-fixture-ok\");\n")
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "data-mate-fixture-ok") {
		t.Fatalf("owned fixture script failed: %v\n%.2048s", err, output)
	}
}

// newDriver binds a driver to its own registry, closed when the test ends.
func newDriver(t *testing.T) (*mongodb.Driver, *pool.Registry) {
	t.Helper()
	r, err := pool.New(pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return mongodb.New(r), r
}

func requireCode(t *testing.T, err error, code contracts.Code) {
	t.Helper()
	var e *database.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

// codecCase is one testdata/codecs.json entry; the integration test cannot use
// the offline loader in package mongodb.
type codecCase struct {
	Name    string          `json:"name"`
	ExtJSON json.RawMessage `json:"extjson,omitempty"`
	JSON    json.RawMessage `json:"json,omitempty"`
	Seed    *bool           `json:"seed,omitempty"`
}

// seededCodecs returns the corpus cases the live fixture stores: Extended JSON
// values not marked "seed": false.
func seededCodecs(t *testing.T) []codecCase {
	t.Helper()
	b, err := os.ReadFile("testdata/codecs.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version int         `json:"version"`
		Cases   []codecCase `json:"cases"`
	}
	if err = json.Unmarshal(b, &doc); err != nil || doc.Version != 1 {
		t.Fatal("invalid codec fixtures", err)
	}
	var out []codecCase
	for _, c := range doc.Cases {
		if len(c.ExtJSON) > 0 && (c.Seed == nil || *c.Seed) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatal("no seeded codec cases")
	}
	return out
}

// compact normalizes JSON text without reordering keys or merging duplicates.
func compact(t *testing.T, name string, b []byte) string {
	t.Helper()
	out, err := json.Marshal(json.RawMessage(b))
	if err != nil {
		t.Fatalf("%s: invalid JSON: %v", name, err)
	}
	return string(out)
}

// Offline fakes cannot prove SCRAM against a real server, the server's own
// privilege expansion for built-in and custom roles, authorization enforcement,
// cursor cleanup or Extended JSON round trips. Run this scenario only against
// the container created by make test-integration DB_DRIVER=mongodb.
func TestMongoDBIntegration(t *testing.T) {
	path := os.Getenv("DATA_MATE_MONGODB_FIXTURE")
	if path == "" {
		t.Skip("requires the owned MongoDB Docker fixture: make test-integration DB_DRIVER=mongodb")
	}
	if os.Getenv("CI") != "" {
		t.Fatal("integration is excluded from CI")
	}
	var m struct {
		Root, Container, Owner, Image, Driver string
		Port                                  int
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &m) != nil || m.Driver != "mongodb" ||
		!strings.HasPrefix(m.Owner, "dm-mongo-") || m.Container != m.Owner+"-db" || m.Root != filepath.Dir(path) {
		t.Fatal("invalid MongoDB fixture manifest")
	}
	docker := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(t.Context(), "docker", args...).Output()
		if err != nil {
			t.Fatal("owned fixture operation failed", err)
		}
		return strings.TrimSpace(string(output))
	}
	// A manually supplied endpoint cannot turn this test into an external DB runner.
	if docker("inspect", "--format", `{{index .Config.Labels "com.data-mate.fixture"}}`, m.Container) != m.Owner ||
		docker("port", m.Container, "27017/tcp") != "127.0.0.1:"+strconv.Itoa(m.Port) {
		t.Fatal("MongoDB fixture ownership or endpoint mismatch")
	}
	value, ok := strings.CutPrefix(docker("port", m.Container, "27018/tcp"), "127.0.0.1:")
	noauth, err := strconv.Atoi(value)
	if !ok || err != nil || noauth <= 0 {
		t.Fatal("unexpected authorization-disabled fixture binding")
	}
	f := &fixture{root: m.Root, container: m.Container, image: m.Image, port: m.Port, noauth: noauth}
	raw, err := os.ReadFile(filepath.Join(m.Root, "reader-password"))
	if err != nil {
		t.Fatal("fixture credential unavailable")
	}
	// Passwords are random hex, used only in memory, never command arguments/logs.
	f.password = strings.TrimSpace(string(raw))
	if len(f.password) != 64 || strings.Trim(f.password, "0123456789abcdef") != "" {
		t.Fatal("invalid synthetic password")
	}
	codecs := seededCodecs(t)
	type seedCase struct {
		Name    string          `json:"name"`
		ExtJSON json.RawMessage `json:"extjson"`
	}
	seeds := make([]seedCase, len(codecs))
	for i, c := range codecs {
		seeds[i] = seedCase{c.Name, c.ExtJSON}
	}
	cases, err := json.Marshal(seeds)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile("testdata/seed.js")
	if err != nil {
		t.Fatal(err)
	}
	f.mongosh(t, "const codecCases = "+string(cases)+";\n"+string(seed))

	d, r := newDriver(t)
	router := database.NewRouter(r, map[string]database.Operations{"mongodb": d})
	p := f.profile("reader", fixtureID)
	a := f.access(p)
	ready, err := d.Test(t.Context(), a)
	if err != nil || ready.Stage != "read_only" {
		t.Fatalf("reader admission on %s: %+v %v", f.image, ready, err)
	}
	majorMinor := strconv.Itoa(ready.ServerVersion/10000) + "." + strconv.Itoa(ready.ServerVersion/100%100)
	if f.image != "mongo:"+majorMinor {
		t.Fatalf("fixture %s reports MongoDB %s", f.image, majorMinor)
	}
	t.Logf("server version=%d image=%s", ready.ServerVersion, f.image)
	connectionAcceptance(t, f, d, p)
	admissionAcceptance(t, f, d, router, p, ready.ServerVersion)

	desc, err := router.DescribeDatabase(t.Context(), a)
	raw, _ = json.Marshal(desc)
	if err != nil || contracts.Validate("db-describe.output", raw) != nil {
		t.Fatalf("CLI catalog: %.500s %v", raw, err)
	}
	verifyDescription(t, desc)
	codecAcceptance(t, router, a, codecs)
	documentToolAcceptance(t, router, a)
	executionAcceptance(t, f, router, p)
	transportAcceptance(t, f, p)

	page, err := router.ListCollections(t.Context(), a, database.CollectionPageRequest{PageSize: 1})
	if err != nil || page.NextCursor == nil {
		t.Fatal("missing test cursor", err)
	}
	r.Invalidate(p.ID)
	_, err = router.ListCollections(t.Context(), a, database.CollectionPageRequest{PageSize: 1, Cursor: *page.NextCursor})
	requireCode(t, err, contracts.StaleCursor)
}

// connectionAcceptance covers verified TLS on the shared port and the failure
// stages of TLS, wrong passwords and the wrong authentication database.
func connectionAcceptance(t *testing.T, f *fixture, d *mongodb.Driver, p config.Profile) {
	t.Helper()
	secure := p
	secure.Transport.TLS = config.TLS{Mode: "verify-full", CAFile: filepath.Join(f.root, "ca.crt")}
	ready, err := d.Test(t.Context(), f.access(secure))
	if err != nil || !ready.TLS || ready.Stage != "read_only" {
		t.Fatalf("verified TLS: %+v %v", ready, err)
	}
	for _, mode := range []string{"hostname", "ca"} {
		bad := secure
		if mode == "hostname" {
			bad.Connection.Host = "127.0.0.1"
		} else {
			bad.Transport.TLS.CAFile = filepath.Join(f.root, "bad.crt")
		}
		ready, err = d.Test(t.Context(), f.access(bad))
		if ready.Stage != "dial" {
			t.Fatalf("TLS %s failed at %s: %v", mode, ready.Stage, err)
		}
		requireCode(t, err, contracts.ConnectFailed)
	}
	const wrong = "synthetic-wrong-password"
	ready, err = d.Test(t.Context(), database.NewAccess(p, wrong))
	if ready.Stage != "authentication" {
		t.Fatalf("bad password failed at %s: %v", ready.Stage, err)
	}
	requireCode(t, err, contracts.ConnectFailed)
	if strings.Contains(err.Error(), f.password) || strings.Contains(err.Error(), wrong) {
		t.Fatal("credential in error")
	}
	// reader exists only in admin, so SCRAM in app must fail before any audit.
	elsewhere := p
	elsewhere.Connection.Database = "app"
	ready, err = d.Test(t.Context(), f.access(elsewhere))
	if ready.Stage != "authentication" {
		t.Fatalf("wrong authentication database failed at %s: %v", ready.Stage, err)
	}
	requireCode(t, err, contracts.ConnectFailed)
}

// admissionAcceptance proves the audit against the server's own privilege
// expansion of built-in and custom roles, the authorization probe against a
// real authorization-disabled replica-set primary, and warm-pool approval.
func admissionAcceptance(t *testing.T, f *fixture, d *mongodb.Driver, router *database.Router, p config.Profile, version int) {
	t.Helper()
	for _, c := range []struct {
		user, db string
		pass     bool
	}{
		{"reader", "admin", true},
		{"scoped", "app", true},
		{"narrow", "admin", true},
		{"any_reader", "admin", true},
		// clusterMonitor holds only allowed actions from MongoDB 8.2.
		{"monitor", "admin", version >= 80200},
		{"rw", "admin", false},
		{"ddl", "admin", false},
		{"user_admin", "admin", false},
		{"rw_any", "admin", false},
		{"cluster_admin", "admin", false},
		{"backup", "admin", false},
		{"one_insert", "admin", false},
		{"nested", "admin", false},
		{"view_user", "admin", false},
		{"cred_reader", "admin", false},
	} {
		q := p
		q.Connection.Username, q.Connection.Database = c.user, c.db
		ready, err := d.Test(t.Context(), f.access(q))
		if c.pass {
			if err != nil || ready.Stage != "read_only" {
				t.Errorf("%s: admissible account refused at %s: %v", c.user, ready.Stage, err)
			}
			continue
		}
		var safe *database.Error
		if !errors.As(err, &safe) || safe.Code != contracts.ReadOnlyViolation || ready.Stage != "read_only" ||
			strings.Contains(safe.Message, c.user) || strings.Contains(safe.Message, "app") {
			t.Errorf("%s: wanted a redacted READ_ONLY_VIOLATION at read_only, got %s: %v", c.user, ready.Stage, err)
		}
	}
	// The reader authenticates and passes the privilege audit on the
	// authorization-disabled primary; only the probe can refuse it.
	unenforced := p
	unenforced.Connection.Port = f.noauth
	ready, err := d.Test(t.Context(), f.access(unenforced))
	if ready.Stage != "read_only" || err == nil || !strings.Contains(err.Error(), "does not enforce authorization") {
		t.Fatalf("authorization-disabled server at %s: %v", ready.Stage, err)
	}
	requireCode(t, err, contracts.ReadOnlyViolation)
	if t.Failed() {
		t.FailNow()
	}

	// A warm pool keeps approval while connected; fresh diagnostics always re-audit.
	a := f.access(p)
	find := func() error {
		_, e := router.FindDocuments(t.Context(), a, database.FindRequest{Database: "app", Collection: "items", RowLimit: 1})
		return e
	}
	if err = find(); err != nil {
		t.Fatal(err)
	}
	f.mongosh(t, `db.getSiblingDB("admin").grantRolesToUser("reader", [{ role: "readWrite", db: "app" }]);`)
	if err = find(); err != nil {
		t.Fatal("warm pool repeated privilege audit", err)
	}
	_, err = d.Test(t.Context(), a)
	requireCode(t, err, contracts.ReadOnlyViolation)
	router.Invalidate(p.ID)
	requireCode(t, find(), contracts.ReadOnlyViolation)
	f.mongosh(t, `db.getSiblingDB("admin").revokeRolesFromUser("reader", [{ role: "readWrite", db: "app" }]);`)
	if err = find(); err != nil {
		t.Fatal("failed validation poisoned retry", err)
	}
}

// verifyDescription checks db describe: authorized databases are schemas,
// collections, views and time series are tables, and index names carry their
// collection, without system collections or unreadable databases.
func verifyDescription(t *testing.T, desc database.DatabaseDescription) {
	t.Helper()
	var names []string
	kinds := map[string]string{}
	var app *database.SchemaDescription
	for i, s := range desc.Schemas {
		names = append(names, s.Name)
		for _, table := range s.Tables {
			if strings.HasPrefix(table.Name, "system.") {
				t.Fatalf("description exposed %s.%s", s.Name, table.Name)
			}
		}
		if s.Name == "app" {
			app = &desc.Schemas[i]
		}
		if s.Name == "hidden" && len(s.Tables) == 0 {
			t.Fatal("readable catalog missing")
		}
	}
	if desc.Database != "" || !slices.Equal(names, []string{"app", "hidden"}) || app == nil {
		t.Fatalf("described databases %q, database %q", names, desc.Database)
	}
	for _, table := range app.Tables {
		kinds[table.Name] = table.Kind
	}
	want := map[string]string{"items": "collection", "v_items": "view", "metrics": "timeseries", "capped_log": "collection",
		"validated": "collection", "a.b": "collection", "codecs": "collection", "big": "collection", "large": "collection"}
	for name, kind := range want {
		if kinds[name] != kind {
			t.Fatalf("app tables %v: %s is not a %s", kinds, name, kind)
		}
	}
	items := 0
	for _, index := range app.Indexes {
		if index.Table == "items" {
			items++
		}
		if index.Table == "v_items" {
			t.Fatal("view reported an index")
		}
	}
	if items != 11 || !slices.Contains(app.Indexes, database.IndexName{Name: "_id_", Table: "items"}) {
		t.Fatalf("items indexes: %+v", app.Indexes)
	}
}

// codecAcceptance reads every seeded corpus value through the real server and
// production encoder and compares it with the offline expectation.
func codecAcceptance(t *testing.T, router *database.Router, a database.Access, codecs []codecCase) {
	t.Helper()
	res, err := router.FindDocuments(t.Context(), a, database.FindRequest{Database: "app", Collection: "codecs", Sort: json.RawMessage(`{"_id":1}`)})
	if err != nil || res.Truncated || res.DocumentCount != len(codecs) {
		t.Fatalf("codec documents: %d of %d, %v", res.DocumentCount, len(codecs), err)
	}
	got := map[string]json.RawMessage{}
	for _, raw := range res.Documents {
		var doc struct {
			ID string          `json:"_id"`
			V  json.RawMessage `json:"v"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("codec document %.200s: %v", raw, err)
		}
		got[doc.ID] = doc.V
	}
	for _, c := range codecs {
		v, ok := got[c.Name]
		if !ok {
			t.Errorf("%s: not returned", c.Name)
			continue
		}
		if g, w := compact(t, c.Name, v), compact(t, c.Name, c.JSON); g != w {
			t.Errorf("%s: got %s, want %s", c.Name, g, w)
		}
	}
	// An int64 filter beyond 2^53 keeps its exact value through parsing.
	res, err = router.FindDocuments(t.Context(), a, database.FindRequest{Database: "app", Collection: "codecs",
		Filter: json.RawMessage(`{"v":{"$numberLong":"9007199254740993"}}`)})
	if err != nil || res.DocumentCount != 1 {
		t.Fatalf("exact int64 filter matched %d: %v", res.DocumentCount, err)
	}
}

// executionAcceptance covers timeouts, cancellation, a live getMore and
// killCursors, byte truncation and that no server cursor or operation survives.
func executionAcceptance(t *testing.T, f *fixture, router *database.Router, p config.Profile) {
	t.Helper()
	// Every outer document rescans big, far beyond the timeout.
	slow := []json.RawMessage{
		json.RawMessage(`{"$lookup":{"from":"big","let":{"k":"$k"},"pipeline":[{"$match":{"$expr":{"$eq":["$k","$$k"]}}}],"as":"m"}}`),
		json.RawMessage(`{"$count":"n"}`),
	}
	timed := f.profile("reader", "12345678-1234-1234-1234-123456789a01")
	timed.Limits.QueryTimeoutMS = 1000
	started := time.Now()
	_, err := router.AggregateDocuments(t.Context(), f.access(timed), database.AggregateRequest{Database: "app", Collection: "big", Pipeline: slow})
	requireCode(t, err, contracts.QueryTimeout)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	a := f.access(p)
	ctx, cancel := context.WithCancel(t.Context())
	stop := time.AfterFunc(300*time.Millisecond, cancel)
	_, err = router.AggregateDocuments(ctx, a, database.AggregateRequest{Database: "app", Collection: "big", Pipeline: slow})
	stop.Stop()
	cancel()
	requireCode(t, err, contracts.Cancelled)

	// 21 documents of 1 MiB cross the 16 MiB first batch, so the result needs a
	// getMore and leaves a live cursor that must be killed.
	res, err := router.AggregateDocuments(t.Context(), a, database.AggregateRequest{Database: "app", Collection: "large",
		Pipeline: []json.RawMessage{json.RawMessage(`{"$match":{}}`)}, RowLimit: 20})
	if err != nil || res.DocumentCount != 20 || !res.Truncated {
		t.Fatalf("large aggregate: %d truncated=%t %v", res.DocumentCount, res.Truncated, err)
	}
	capped := f.profile("reader", "12345678-1234-1234-1234-123456789a02")
	capped.Limits.MaxResultBytes = 4096
	res, err = router.FindDocuments(t.Context(), f.access(capped), database.FindRequest{Database: "app", Collection: "big", RowLimit: 1000})
	encoded, _ := json.Marshal(res)
	if err != nil || res.DocumentCount == 0 || res.DocumentCount >= 1000 || !res.Truncated || database.ResultBytes(encoded) > 4096 {
		t.Fatalf("byte truncation: %d truncated=%t bytes=%d %v", res.DocumentCount, res.Truncated, database.ResultBytes(encoded), err)
	}
	// Truncated, timed-out and cancelled reads leave no idle cursor or running operation.
	f.mongosh(t, `const deadline = Date.now() + 15000;
for (;;) {
  const ops = db.getSiblingDB("admin").aggregate([
    { $currentOp: { allUsers: true, idleCursors: true } },
    { $match: { ns: { $in: ["app.big", "app.large"] } } },
  ]).toArray();
  if (ops.length === 0) break;
  if (Date.now() > deadline) throw new Error("server cursors or operations survived: " + ops.length);
  sleep(100);
}`)
	res, err = router.FindDocuments(t.Context(), a, database.FindRequest{Database: "app", Collection: "big", RowLimit: 1})
	if err != nil || res.DocumentCount != 1 {
		t.Fatalf("read after cleanup: %v", err)
	}
	t.Log("timeout, cancellation, getMore with killCursors, byte truncation and cursor cleanup passed")
}
