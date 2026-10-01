package sqlguard

import (
	"errors"
	"strings"
	"testing"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// The guard is the MySQL statement-kind gate. Each rejected case names a way
// the lexer could otherwise disagree with the server or admit a non-read form.
func TestQueryGuard(t *testing.T) {
	for _, q := range []string{
		"SELECT 1",
		"select * from app.items where name = 'into' and note = \"for update\"",
		"SELECT `into`, `for` FROM app.`lock` WHERE `lock` IN (1)",
		"SELECT 1;",
		"SELECT 1; -- trailing comment",
		"SELECT 1 /* block */ -- line\r\n",
		"SELECT 1--1",
		"SELECT 'it''s', \"a\\\"b\", 'c\\'d', _utf8mb4'x', X'4D', 0x4D, b'1'",
		"(SELECT 1) UNION (SELECT 2) ORDER BY 1",
		"WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<3) SELECT * FROM c",
		"WITH a AS (SELECT 1), b AS (SELECT 2) (SELECT * FROM a)",
		"WITH RECURSIVE c AS (SELECT 1 AS n) CYCLE n RESTRICT SELECT * FROM c",
		"VALUES ROW(1, 2)",
		"TABLE app.items",
		"SHOW TABLES FROM app",
		"SHOW CREATE TABLE app.items",
		"EXPLAIN SELECT 1",
		"EXPLAIN ANALYZE FORMAT=TREE SELECT * FROM app.items",
		"EXPLAIN FORMAT=JSON WITH a AS (SELECT 1) SELECT * FROM a",
		"DESCRIBE app.items",
		"DESC `app`.`items` name",
		"ANALYZE FORMAT=JSON SELECT 1",
		"SELECT * FROM app.items FOR SYSTEM_TIME ALL",
		"SELECT JSON_EXTRACT(doc, '$.a') -> '$.b', doc->>'$.c' FROM app.docs",
		"SELECT @@session.time_zone, @x = 1",
		"SELECT * FROM app.items WHERE id = ?",
		"SELECT GET_LOCK('n', 0), REPLACE('a', 'a', 'b'), INSERT('abc', 1, 1, 'x')",
	} {
		if _, err := Check(q); err != nil {
			t.Errorf("rejected %q: %v", q, err)
		}
	}
	for _, c := range []struct {
		q    string
		code contracts.Code
	}{
		{"SELECT 1; SELECT 2", contracts.ReadOnlyViolation},
		{"SELECT 1;;", contracts.ReadOnlyViolation},
		{"DELETE FROM app.items", contracts.ReadOnlyViolation},
		{"WITH a AS (SELECT 1) DELETE FROM app.items", contracts.ReadOnlyViolation},
		{"WITH a AS (SELECT 1) UPDATE app.items SET n = 1", contracts.ReadOnlyViolation},
		{"SELECT * INTO OUTFILE '/tmp/x' FROM app.items", contracts.ReadOnlyViolation},
		{"SELECT 1 INTO @x", contracts.ReadOnlyViolation},
		{"SELECT t.into FROM app.t", contracts.ReadOnlyViolation},
		{"SELECT * FROM app.items FOR UPDATE", contracts.ReadOnlyViolation},
		{"SELECT * FROM app.items FOR /* c */ SHARE", contracts.ReadOnlyViolation},
		{"SELECT * FROM app.items LOCK IN SHARE MODE", contracts.ReadOnlyViolation},
		{"SELECT @x := 1", contracts.ReadOnlyViolation},
		{"SET SESSION sql_mode = 'ANSI_QUOTES'", contracts.ReadOnlyViolation},
		{"USE app", contracts.ReadOnlyViolation},
		{"CALL app.p()", contracts.ReadOnlyViolation},
		{"DO SLEEP(1)", contracts.ReadOnlyViolation},
		{"HANDLER app.items OPEN", contracts.ReadOnlyViolation},
		{"LOCK TABLES app.items READ", contracts.ReadOnlyViolation},
		{"ANALYZE TABLE app.items", contracts.ReadOnlyViolation},
		{"EXPLAIN FOR CONNECTION 1", contracts.ReadOnlyViolation},
		{"EXPLAIN UPDATE app.items SET n = 1", contracts.ReadOnlyViolation},
		{"EXPLAIN ANALYZE DELETE FROM app.items", contracts.ReadOnlyViolation},
		{"DESCRIBE app.items name extra", contracts.ReadOnlyViolation},
		// Executable comments run on the server, so they are never treated as text.
		{"SELECT 1 /*!50000 , (SELECT 2) */", contracts.QueryUnsupported},
		{"SELECT 1 /*M!100500 INTO OUTFILE '/tmp/x' */", contracts.QueryUnsupported},
		{"SELECT /*+ SET_VAR(sql_mode='ANSI_QUOTES') */ 1", contracts.QueryUnsupported},
		// A lone CR ends a comment for this lexer but not for the server.
		{"WITH a AS (SELECT 1) # c\r SELECT\nDELETE FROM app.items", contracts.InvalidArgument},
		{"SELECT 1 --\x01x", contracts.InvalidArgument},
		{"SELECT 'unterminated", contracts.InvalidArgument},
		{"SELECT `unterminated", contracts.InvalidArgument},
		{"SELECT 1 /* unterminated", contracts.InvalidArgument},
		{"SELECT \\N", contracts.InvalidArgument},
		{"SELECT (1", contracts.InvalidArgument},
		{"SELECT 1)", contracts.InvalidArgument},
		{";", contracts.InvalidArgument},
		{"SELECT '\xff'", contracts.InvalidArgument},
		{"SELECT 1\x00", contracts.InvalidArgument},
		{strings.Repeat("(", 65) + "SELECT 1" + strings.Repeat(")", 65), contracts.ResourceLimit},
		{"SELECT " + strings.Repeat("1,", 8192) + "1", contracts.ResourceLimit},
		{strings.Repeat(" ", 64<<10+1), contracts.ResourceLimit},
	} {
		_, err := Check(c.q)
		var e *database.Error
		if !errors.As(err, &e) || e.Code != c.code {
			t.Errorf("%q: wanted %s, got %v", c.q, c.code, err)
		}
	}
	// One trailing delimiter is removed before the server prepares the statement.
	if statement, err := Check("SELECT 1 ; /* done */"); err != nil || statement != "SELECT 1 " {
		t.Fatalf("trailing delimiter: %q %v", statement, err)
	}
}

func FuzzQueryGuard(f *testing.F) {
	for _, seed := range []string{"SELECT 1", "WITH a AS (SELECT 1) SELECT * FROM a", "SELECT '\\'' -- x\n", "/*!SELECT*/", "DESCRIBE `a``b`.c"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, q string) {
		if len(q) > 4096 {
			t.Skip()
		}
		statement, err := Check(q)
		if err == nil && !strings.HasPrefix(q, statement) {
			t.Fatal("guard rewrote the statement")
		}
	})
}
