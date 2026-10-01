package sqlguard

import (
	"strings"
	"unicode/utf8"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

func readOnly() error {
	return database.Fail(contracts.ReadOnlyViolation, "only one read query is allowed", false)
}

// tableFormKeywords cannot be a table name in DESCRIBE's table form; they would
// instead start another statement or clause.
var tableFormKeywords = []string{"SELECT", "WITH", "VALUES", "TABLE", "INSERT", "UPDATE", "DELETE", "REPLACE", "FOR", "INTO", "CALL", "DO", "SET", "LOAD", "HANDLER", "LOCK", "UNLOCK", "ANALYZE", "FORMAT", "EXTENDED", "PARTITIONS"}

// Check bounds and validates one read statement without reproducing MySQL
// semantics. It returns the statement to prepare: the input without one
// optional trailing semicolon.
func Check(sql string) (string, error) {
	if !utf8.ValidString(sql) {
		return "", invalid()
	}
	tokens, err := Tokens(sql)
	if err != nil {
		return "", err
	}
	depth := 0
	for _, t := range tokens {
		if t.Kind != Symbol {
			continue
		}
		switch t.Text {
		case "(":
			depth++
			if depth > 64 {
				return "", resource()
			}
		case ")":
			depth--
			if depth < 0 {
				return "", invalid()
			}
		}
	}
	if depth != 0 {
		return "", invalid()
	}
	statement := sql
	for i, t := range tokens {
		if t.Kind == Symbol && t.Text == ";" {
			if i != len(tokens)-1 {
				return "", readOnly()
			}
			statement, tokens = sql[:t.Pos], tokens[:i]
		}
	}
	if len(tokens) == 0 {
		return "", invalid()
	}
	for i, t := range tokens {
		next := func(word string) bool { return i+1 < len(tokens) && tokens[i+1].Is(word) }
		// Writes to files or variables, session assignment and locking reads are
		// rejected wherever they appear; only quoted identifiers may use these names.
		if t.Is("INTO") || (t.Is("FOR") && (next("UPDATE") || next("SHARE"))) || (t.Is("LOCK") && next("IN")) || (t.Kind == Symbol && t.Text == ":=") {
			return "", readOnly()
		}
	}
	g := guard{tokens: tokens}
	switch first := tokens[0]; {
	case first.Is("SHOW"):
		return statement, nil
	case first.Is("EXPLAIN") || first.Is("DESCRIBE") || first.Is("DESC"):
		err = g.explain(1)
	case first.Is("ANALYZE"):
		err = g.analyze(1)
	default:
		err = g.selectFamily(0)
	}
	if err != nil {
		return "", err
	}
	return statement, nil
}

type guard struct{ tokens []Token }

func (g guard) at(i int) Token {
	if i < len(g.tokens) {
		return g.tokens[i]
	}
	return Token{Kind: Symbol}
}

func (g guard) symbol(i int, s string) bool {
	t := g.at(i)
	return i < len(g.tokens) && t.Kind == Symbol && t.Text == s
}

// skipGroup returns the index after the parenthesized group starting at i.
func (g guard) skipGroup(i int) (int, error) {
	if !g.symbol(i, "(") {
		return 0, invalid()
	}
	depth := 0
	for ; i < len(g.tokens); i++ {
		switch {
		case g.symbol(i, "("):
			depth++
		case g.symbol(i, ")"):
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, invalid()
}

// selectFamily accepts SELECT, VALUES, TABLE, a parenthesized query or a WITH
// clause whose main statement is one of those.
func (g guard) selectFamily(i int) error {
	for g.symbol(i, "(") {
		i++
	}
	switch t := g.at(i); {
	case t.Is("SELECT") || t.Is("VALUES") || t.Is("TABLE"):
		return nil
	case t.Is("WITH"):
		return g.with(i + 1)
	}
	return readOnly()
}

// with checks a WITH clause structurally: MySQL and MariaDB allow a CTE list
// before UPDATE and DELETE, so the main statement must be a read query.
func (g guard) with(i int) error {
	if g.at(i).Is("RECURSIVE") {
		i++
	}
	for {
		if t := g.at(i); i >= len(g.tokens) || (t.Kind != Word && t.Kind != Identifier) {
			return invalid()
		}
		i++
		var err error
		if g.symbol(i, "(") {
			if i, err = g.skipGroup(i); err != nil {
				return err
			}
		}
		if !g.at(i).Is("AS") {
			return invalid()
		}
		if i, err = g.skipGroup(i + 1); err != nil {
			return err
		}
		if g.at(i).Is("CYCLE") {
			// MariaDB: CYCLE column [, column] RESTRICT
			for i++; i < len(g.tokens) && !g.at(i).Is("RESTRICT"); i++ {
			}
			if i == len(g.tokens) {
				return invalid()
			}
			i++
		}
		if !g.symbol(i, ",") {
			return g.selectFamily(i)
		}
		i++
	}
}

// explain accepts EXPLAIN or DESCRIBE of a read query, or DESCRIBE's table form,
// an alias of SHOW COLUMNS. EXPLAIN ANALYZE executes its query, as on PostgreSQL.
func (g guard) explain(i int) error {
	for {
		switch t := g.at(i); {
		case t.Is("ANALYZE") || t.Is("EXTENDED") || t.Is("PARTITIONS"):
			i++
			continue
		case t.Is("FORMAT"):
			if !g.symbol(i+1, "=") || g.at(i+2).Kind != Word {
				return invalid()
			}
			i += 3
			continue
		}
		break
	}
	if t := g.at(i); g.symbol(i, "(") || t.Is("SELECT") || t.Is("VALUES") || t.Is("TABLE") || t.Is("WITH") {
		return g.selectFamily(i)
	}
	return g.tableForm(i)
}

// tableForm accepts [schema.]table [column | 'pattern'] and nothing else.
func (g guard) tableForm(i int) error {
	name := func(i int) bool {
		t := g.at(i)
		if i >= len(g.tokens) {
			return false
		}
		if t.Kind == Identifier {
			return true
		}
		if t.Kind != Word {
			return false
		}
		for _, k := range tableFormKeywords {
			if t.Is(k) {
				return false
			}
		}
		return true
	}
	if !name(i) {
		return readOnly()
	}
	i++
	if g.symbol(i, ".") {
		if !name(i + 1) {
			return readOnly()
		}
		i += 2
	}
	if i < len(g.tokens) && (name(i) || g.at(i).Kind == String) {
		i++
	}
	if i != len(g.tokens) {
		return readOnly()
	}
	return nil
}

// analyze accepts MariaDB's ANALYZE [FORMAT=JSON] of a SELECT query; ANALYZE
// TABLE and other maintenance forms are rejected.
func (g guard) analyze(i int) error {
	if g.at(i).Is("FORMAT") {
		if !g.symbol(i+1, "=") || g.at(i+2).Kind != Word {
			return invalid()
		}
		i += 3
	}
	if t := g.at(i); !g.symbol(i, "(") && !t.Is("SELECT") && !t.Is("WITH") {
		return readOnly()
	}
	return g.selectFamily(i)
}

// Quote returns a backtick-quoted identifier. Callers validate names first.
func Quote(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }
