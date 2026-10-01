// Package sqlguard checks MySQL and MariaDB read statement kinds with a bounded
// lexer. It deliberately does not parse SQL: the audited read-only account and
// read-only transaction are the primary boundary, and the server parses every
// accepted statement. Where this lexer could disagree with the server's lexing
// under the pinned SQL mode, the input is rejected instead of guessed.
package sqlguard

import (
	"strings"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// Kind classifies one token.
type Kind int

// Token kinds; comments and whitespace are never returned.
const (
	// Word is an unquoted identifier, keyword or number-like run.
	Word Kind = iota
	// Identifier is a backtick-quoted identifier; Text holds its unquoted name.
	Identifier
	// String is a single- or double-quoted literal; Text holds its raw body.
	String
	// Symbol is punctuation or an operator; Text holds its spelling.
	Symbol
	// Placeholder is a ? parameter marker.
	Placeholder
)

// Token is one lexical unit. Pos is its byte offset in the input.
type Token struct {
	Kind Kind
	Text string
	Pos  int
}

// Is reports whether t is the unquoted keyword word, compared in ASCII only,
// as the server compares keywords.
func (t Token) Is(word string) bool {
	if t.Kind != Word || len(t.Text) != len(word) {
		return false
	}
	for i := 0; i < len(word); i++ {
		c := t.Text[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != word[i] {
			return false
		}
	}
	return true
}

const (
	maxSQLBytes = 64 << 10
	maxTokens   = 8192
)

func invalid() error { return database.Fail(contracts.InvalidArgument, "invalid SQL", false) }
func resource() error {
	return database.Fail(contracts.ResourceLimit, "SQL complexity exceeds limits", false)
}
func unsupported() error {
	return database.Fail(contracts.QueryUnsupported, "executable comments and optimizer hints are not supported", false)
}

func wordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// lineComment returns the index after a # or -- comment body, which ends at LF.
// A lone CR would end the comment for this lexer but not for the server.
func lineComment(s string, i int) (int, error) {
	for ; i < len(s); i++ {
		switch s[i] {
		case '\n':
			return i, nil
		case '\r':
			if i+1 >= len(s) || s[i+1] != '\n' {
				return 0, invalid()
			}
		}
	}
	return i, nil
}

// Tokens lexes SQL as MySQL and MariaDB do with ANSI_QUOTES and
// NO_BACKSLASH_ESCAPES disabled, which the driver pins for every operation.
// Executable (/*! and /*M!) comments and optimizer hints (/*+) are rejected.
func Tokens(s string) ([]Token, error) {
	if len(s) > maxSQLBytes {
		return nil, resource()
	}
	if strings.ContainsRune(s, 0) {
		return nil, invalid()
	}
	var out []Token
	emit := func(t Token) error {
		if len(out) == maxTokens {
			return resource()
		}
		out = append(out, t)
		return nil
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '#':
			end, err := lineComment(s, i+1)
			if err != nil {
				return nil, err
			}
			i = end
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			// "--" starts a comment only before space, tab, LF or the end of
			// input; other control characters are ambiguous and rejected.
			if i+2 == len(s) {
				i += 2
				continue
			}
			next := s[i+2]
			switch {
			case next == ' ' || next == '\t':
				end, err := lineComment(s, i+3)
				if err != nil {
					return nil, err
				}
				i = end
			case next == '\n':
				i += 2
			case next < 0x20 || next == 0x7f:
				return nil, invalid()
			default:
				if err := emit(Token{Kind: Symbol, Text: "-", Pos: i}); err != nil {
					return nil, err
				}
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			body := s[i+2:]
			if strings.HasPrefix(body, "!") || strings.HasPrefix(body, "M!") || strings.HasPrefix(body, "+") {
				return nil, unsupported()
			}
			end := strings.Index(body, "*/")
			if end < 0 {
				return nil, invalid()
			}
			i += 2 + end + 2
		case c == '\'' || c == '"':
			start := i
			i++
			for {
				if i >= len(s) {
					return nil, invalid()
				}
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == c {
					if i+1 < len(s) && s[i+1] == c {
						i += 2
						continue
					}
					break
				}
				i++
			}
			if err := emit(Token{Kind: String, Text: s[start+1 : i], Pos: start}); err != nil {
				return nil, err
			}
			i++
		case c == '`':
			start := i
			var name strings.Builder
			i++
			for {
				if i >= len(s) {
					return nil, invalid()
				}
				if s[i] == '`' {
					if i+1 < len(s) && s[i+1] == '`' {
						name.WriteByte('`')
						i += 2
						continue
					}
					break
				}
				name.WriteByte(s[i])
				i++
			}
			if err := emit(Token{Kind: Identifier, Text: name.String(), Pos: start}); err != nil {
				return nil, err
			}
			i++
		case wordByte(c):
			start := i
			for i < len(s) && wordByte(s[i]) {
				i++
			}
			if err := emit(Token{Kind: Word, Text: s[start:i], Pos: start}); err != nil {
				return nil, err
			}
		case c == '?':
			if err := emit(Token{Kind: Placeholder, Text: "?", Pos: i}); err != nil {
				return nil, err
			}
			i++
		case c == '\\' || c < 0x20 || c == 0x7f:
			return nil, invalid()
		default:
			text := s[i : i+1]
			for _, op := range []string{"<=>", "->>", ":=", "<=", ">=", "<>", "!=", "<<", ">>", "&&", "||", "->", "@@"} {
				if strings.HasPrefix(s[i:], op) {
					text = op
					break
				}
			}
			if err := emit(Token{Kind: Symbol, Text: text, Pos: i}); err != nil {
				return nil, err
			}
			i += len(text)
		}
	}
	return out, nil
}
