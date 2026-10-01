package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

func profile(f Flavor) config.Profile {
	return config.Profile{ID: "12345678-1234-1234-1234-123456789abc", Alias: "fixture", Driver: f.Name(), Connection: config.Connection{Host: "localhost", Port: 3306, Username: "reader"}, Transport: config.Transport{TLS: config.TLS{Mode: "disabled"}}}
}

// driverWith binds a driver to its own registry, closed when the test ends.
func driverWith(t *testing.T, f Flavor) *Driver {
	t.Helper()
	r, err := pool.New(pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return New(r, f)
}

func requireCode(t *testing.T, err error, code contracts.Code) {
	t.Helper()
	var e *database.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

// fixture is one owned server; admin runs fixture SQL as the image's root account.
type fixture struct {
	flavor   Flavor
	image    string
	root     string
	port     int
	password string
	admin    *sql.DB
}

func (f *fixture) sql(t *testing.T, statements ...string) {
	t.Helper()
	for _, q := range statements {
		if _, err := f.admin.ExecContext(t.Context(), q); err != nil {
			var my *mysql.MySQLError
			if errors.As(err, &my) {
				t.Fatalf("fixture SQL failed (%d): %s", my.Number, q)
			}
			t.Fatal("fixture SQL failed", err)
		}
	}
}

func (f *fixture) scalar(t *testing.T, q string) string {
	t.Helper()
	var v sql.NullString
	if err := f.admin.QueryRowContext(t.Context(), q).Scan(&v); err != nil {
		t.Fatal("fixture query failed", err)
	}
	return v.String
}

// No PostgreSQL scenario exercises a MySQL-family server; this scenario owns the
// driver's acceptance on every image of the matrix.
func TestMySQLIntegration(t *testing.T) {
	path := os.Getenv("DATA_MATE_MYSQL_FIXTURE")
	if path == "" {
		t.Skip("requires owned Docker fixture: make test-integration DB_DRIVER=mysql or DB_DRIVER=mariadb")
	}
	if os.Getenv("CI") != "" {
		t.Fatal("integration is excluded from CI")
	}
	var manifest struct {
		Port                                  int
		Root, Container, Owner, Image, Driver string
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &manifest) != nil {
		t.Fatal("invalid fixture manifest")
	}
	// A manually supplied endpoint cannot turn this test into an external DB runner.
	b, err = exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "com.data-mate.fixture"}}`, manifest.Container).Output()
	if err != nil || strings.TrimSpace(string(b)) != manifest.Owner || !strings.HasPrefix(manifest.Owner, "dm-my-") {
		t.Fatal("fixture ownership mismatch")
	}
	b, err = exec.Command("docker", "port", manifest.Container, "3306/tcp").Output()
	if err != nil || strings.TrimSpace(string(b)) != "127.0.0.1:"+strconv.Itoa(manifest.Port) {
		t.Fatal("fixture endpoint mismatch")
	}
	f := &fixture{flavor: MySQL, image: manifest.Image, root: manifest.Root, port: manifest.Port}
	if manifest.Driver == "mariadb" {
		f.flavor = MariaDB
	}
	secret := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(manifest.Root, name))
		if err != nil {
			t.Fatal("fixture credential unavailable")
		}
		return strings.TrimSpace(string(b))
	}
	f.password = secret("reader-password")
	// Passwords are random hex, used only in memory, never command arguments/logs.
	if len(f.password) != 64 || strings.Trim(f.password, "0123456789abcdef") != "" {
		t.Fatal("invalid synthetic password")
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = "root", secret("admin-password"), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(manifest.Port))
	cfg.Logger = &mysql.NopLogger{}
	cfg.MultiStatements = true
	cfg.Params = map[string]string{"sql_mode": "''", "time_zone": "'+00:00'"}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.admin = sql.OpenDB(connector)
	defer f.admin.Close()
	f.sql(t, "CREATE USER 'reader'@'%' IDENTIFIED BY '"+f.password+"'")
	f.sql(t,
		"CREATE DATABASE app", "CREATE DATABASE hidden", "CREATE DATABASE `Dot.Schema`", "CREATE DATABASE secret",
		"CREATE TABLE hidden.target(id INT PRIMARY KEY)", "INSERT INTO hidden.target VALUES (1)",
		`CREATE TABLE app.items(
 id INT AUTO_INCREMENT PRIMARY KEY,
 target INT,
 name VARCHAR(32) NOT NULL DEFAULT 'it''s',
 amount DECIMAL(10,2) DEFAULT 0,
 doubled DECIMAL(12,2) GENERATED ALWAYS AS (amount * 2) VIRTUAL,
 updated TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
 UNIQUE KEY items_name (name),
 KEY items_prefix (name(4)),
 CONSTRAINT items_target FOREIGN KEY (target) REFERENCES hidden.target(id) ON DELETE CASCADE,
 CONSTRAINT items_amount CHECK (amount >= 0)) ENGINE=InnoDB`,
		"INSERT INTO app.items(target, name, amount) VALUES (1,'one',1.50),(NULL,'two',2),(NULL,'three',3),(NULL,'four',4),(NULL,'five',5),(NULL,'six',6),(NULL,'seven',7),(NULL,'eight',8),(NULL,'nine',9),(NULL,'ten',10)",
		"CREATE VIEW app.v AS SELECT id, name FROM app.items",
		"CREATE TABLE app.counter_log(n INT)", "INSERT INTO app.counter_log VALUES (0)",
		"CREATE FUNCTION app.double_it(x INT) RETURNS INT DETERMINISTIC NO SQL RETURN x * 2",
		"CREATE PROCEDURE app.report(IN lim INT, OUT total INT) READS SQL DATA SELECT COUNT(*) INTO total FROM app.items",
		"CREATE FUNCTION app.bump() RETURNS INT DETERMINISTIC MODIFIES SQL DATA BEGIN UPDATE app.counter_log SET n = n + 1; RETURN 1; END",
		"CREATE TABLE `Dot.Schema`.`a.b`(id INT)", "CREATE TABLE secret.hidden_rows(id INT)",
		"CREATE ROLE writer, viewer, bridge", "GRANT INSERT ON app.* TO writer", "GRANT SELECT ON hidden.* TO viewer",
		"GRANT SELECT, SHOW VIEW, EXECUTE ON app.* TO 'reader'@'%'", "GRANT SELECT ON hidden.* TO 'reader'@'%'", "GRANT SELECT ON `Dot.Schema`.* TO 'reader'@'%'",
	)
	if f.flavor == MySQL {
		f.sql(t, "GRANT SHOW_ROUTINE ON *.* TO 'reader'@'%'")
	} else {
		f.sql(t, "CREATE SEQUENCE app.counter START WITH 5 INCREMENT BY 2")
	}
	p := profile(f.flavor)
	p.Connection.Port = f.port
	access := database.NewAccess(p, f.password)
	d := driverWith(t, f.flavor)
	ready, err := d.Test(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Stage != "read_only" || len(ready.Stages) != 5 {
		t.Fatalf("readiness stages: %+v", ready)
	}
	t.Logf("server version=%d image=%s", ready.ServerVersion, f.image)
	want := map[string]int{"mysql:8.4": 804, "mysql:9.7": 907, "mariadb:10.11": 1011, "mariadb:12.3": 1203}[f.image]
	if want == 0 || ready.ServerVersion/100 != want {
		t.Fatal("server/image version mismatch")
	}
	// The other flavor's driver reaches the server but refuses it with a hint.
	other := MariaDB
	if f.flavor == MariaDB {
		other = MySQL
	}
	mismatched := profile(other)
	mismatched.Connection.Port = f.port
	ready, err = driverWith(t, other).Test(t.Context(), database.NewAccess(mismatched, f.password))
	requireCode(t, err, contracts.QueryUnsupported)
	if ready.Stage != "version" || !strings.Contains(err.Error(), "use driver "+f.flavor.Name()) {
		t.Fatalf("flavor mismatch at %s: %v", ready.Stage, err)
	}
	tlsProfile := p
	tlsProfile.Transport.TLS = config.TLS{Mode: "verify-full", CAFile: filepath.Join(f.root, "ca.crt")}
	ready, err = d.Test(t.Context(), database.NewAccess(tlsProfile, f.password))
	if err != nil || !ready.TLS {
		t.Fatalf("verified TLS: %v", err)
	}
	for _, mode := range []string{"hostname", "ca"} {
		bad := tlsProfile
		if mode == "hostname" {
			bad.Connection.Host = "127.0.0.1"
		} else {
			bad.Transport.TLS.CAFile = filepath.Join(f.root, "bad.crt")
		}
		ready, err = d.Test(t.Context(), database.NewAccess(bad, f.password))
		if ready.Stage != "dial" {
			t.Fatalf("TLS %s failed at %s: %v", mode, ready.Stage, err)
		}
		requireCode(t, err, contracts.ConnectFailed)
	}
	ready, err = d.Test(t.Context(), database.NewAccess(p, "synthetic-wrong-password"))
	if ready.Stage != "authentication" {
		t.Fatalf("bad password failed at %s: %v", ready.Stage, err)
	}
	requireCode(t, err, contracts.ConnectFailed)
	if strings.Contains(err.Error(), f.password) || strings.Contains(err.Error(), "synthetic-wrong-password") {
		t.Fatal("credential in error")
	}
	accountAcceptance(t, f, d, access)
	queryAcceptance(t, f, d, access)
	codecAcceptance(t, f, d, access)
	catalogAcceptance(t, f, d, access)
	mcpAcceptance(t, d, access)
	transportAcceptance(t, f, p)
	cliDiagnosticsAcceptance(t, f.root)
	d.pools.Close()
	_, err = d.Test(t.Context(), access)
	requireCode(t, err, contracts.ServiceUnavailable)
}

// The grant matrix proves collection and classification on the real server,
// including roles reachable through SET ROLE, which the account may activate.
// PROXY needs a grantor the images do not provide; the offline corpus covers it.
func accountAcceptance(t *testing.T, f *fixture, d *Driver, a database.Access) {
	t.Helper()
	cases := []struct{ grant, revoke []string }{
		{[]string{"GRANT INSERT ON app.* TO 'reader'@'%'"}, []string{"REVOKE INSERT ON app.* FROM 'reader'@'%'"}},
		{[]string{"GRANT UPDATE (name) ON app.items TO 'reader'@'%'"}, []string{"REVOKE UPDATE (name) ON app.items FROM 'reader'@'%'"}},
		{[]string{"GRANT CREATE ON hidden.* TO 'reader'@'%'"}, []string{"REVOKE CREATE ON hidden.* FROM 'reader'@'%'"}},
		{[]string{"GRANT FILE ON *.* TO 'reader'@'%'"}, []string{"REVOKE FILE ON *.* FROM 'reader'@'%'"}},
		{[]string{"GRANT CREATE TEMPORARY TABLES ON app.* TO 'reader'@'%'"}, []string{"REVOKE CREATE TEMPORARY TABLES ON app.* FROM 'reader'@'%'"}},
		{[]string{"GRANT TRIGGER ON app.items TO 'reader'@'%'"}, []string{"REVOKE TRIGGER ON app.items FROM 'reader'@'%'"}},
		{[]string{"GRANT GRANT OPTION ON hidden.* TO 'reader'@'%'"}, []string{"REVOKE GRANT OPTION ON hidden.* FROM 'reader'@'%'"}},
		{[]string{"GRANT writer TO 'reader'@'%'"}, []string{"REVOKE writer FROM 'reader'@'%'"}},
		{[]string{"GRANT writer TO bridge", "GRANT bridge TO 'reader'@'%'"}, []string{"REVOKE bridge FROM 'reader'@'%'", "REVOKE writer FROM bridge"}},
		{[]string{"GRANT viewer TO 'reader'@'%' WITH ADMIN OPTION"}, []string{"REVOKE viewer FROM 'reader'@'%'"}},
	}
	if f.flavor == MySQL {
		cases = append(cases,
			struct{ grant, revoke []string }{[]string{"GRANT BACKUP_ADMIN ON *.* TO 'reader'@'%'"}, []string{"REVOKE BACKUP_ADMIN ON *.* FROM 'reader'@'%'"}},
			struct{ grant, revoke []string }{[]string{"SET GLOBAL mandatory_roles = 'writer'"}, []string{"SET GLOBAL mandatory_roles = ''"}},
		)
	} else {
		cases = append(cases, struct{ grant, revoke []string }{[]string{"GRANT INSERT ON app.* TO PUBLIC"}, []string{"REVOKE INSERT ON app.* FROM PUBLIC"}})
	}
	for i, c := range cases {
		f.sql(t, c.grant...)
		_, err := d.Test(t.Context(), a)
		f.sql(t, c.revoke...)
		var failure *database.Error
		if !errors.As(err, &failure) || failure.Code != contracts.ReadOnlyViolation {
			t.Fatalf("account case %d %q: %v", i, c.grant, err)
		}
	}
	// Read-only roles and monitoring privileges convey no write capability.
	f.sql(t, "GRANT viewer TO 'reader'@'%'", "GRANT PROCESS ON *.* TO 'reader'@'%'")
	_, err := d.Test(t.Context(), a)
	f.sql(t, "REVOKE viewer FROM 'reader'@'%'", "REVOKE PROCESS ON *.* FROM 'reader'@'%'")
	if err != nil {
		t.Fatal("read-only role and PROCESS rejected", err)
	}
	// A warm pool keeps approval while connected; fresh diagnostics always re-audit.
	query := func() error {
		_, e := d.Query(t.Context(), a, database.QueryRequest{SQL: "SELECT 1"})
		return e
	}
	if err = query(); err != nil {
		t.Fatal(err)
	}
	f.sql(t, "GRANT INSERT ON app.* TO 'reader'@'%'")
	if err = query(); err != nil {
		t.Fatal("warm pool repeated privilege audit", err)
	}
	_, err = d.Test(t.Context(), a)
	requireCode(t, err, contracts.ReadOnlyViolation)
	d.pools.Invalidate(a.Profile.ID)
	requireCode(t, query(), contracts.ReadOnlyViolation)
	f.sql(t, "REVOKE INSERT ON app.* FROM 'reader'@'%'")
	if err = query(); err != nil {
		t.Fatal("failed validation poisoned retry", err)
	}
}

func cliDiagnosticsAcceptance(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-count=1", "-timeout=55s", "-run", "^TestConnectionDiagnostics$", "./internal/cli")
	cmd.Dir = filepath.Join("..", "..", "..")
	cmd.Env = append(os.Environ(), "DATA_MATE_CLI_DIAGNOSTICS="+filepath.Join(root, "manifest.json"), "GOPROXY=off", "GOSUMDB=off")
	cmd.WaitDelay = time.Second
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CLI diagnostic fixture: %v\n%s", err, output)
	}
	t.Log("CLI diagnostic fixture: real driver, encrypted synthetic credentials, batch/single JSON and auth/TLS/read-only stages passed")
}
