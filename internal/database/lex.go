package database

import "strings"

// tokenType is the lexical class of a SQL token.
//
// The distinction that matters for the permission gate is between things that
// can name a statement verb or a column (tWord, tQuoted) and things that cannot
// (tString, tNumber, tPunct). Hiding a verb inside a literal must not fool the
// classifier, and a predicate built only from literals cannot bound a DELETE.
type tokenType uint8

const (
	// tWord is a bare identifier or keyword. Text is lower-cased.
	tWord tokenType = iota
	// tQuoted is a quoted identifier ("x", `x`, [x]). Text is the inner text.
	// It can name a column but can never be a statement verb.
	tQuoted
	// tString is a string literal. Its content is discarded.
	tString
	// tNumber is a numeric literal.
	tNumber
	// tPunct is an operator or separator. Text is the operator.
	tPunct
	// tSemi is a statement separator.
	tSemi
)

type token struct {
	Type tokenType
	Text string
}

// is reports whether the token is the given bare keyword.
func (t token) is(word string) bool {
	return t.Type == tWord && t.Text == word
}

// dialect describes the lexical rules that differ between engines.
//
// These are not stylistic differences: the same bytes are code on one engine
// and a comment or a string on another, so a statement can be a harmless read
// under one reading and a batch containing DROP TABLE under another.
type dialect struct {
	// BackslashEscapes makes \' an escape inside a string literal. MySQL does
	// this; PostgreSQL with standard_conforming_strings does not. It decides
	// whether `'a\'; DROP TABLE t; --'` is one literal or a literal followed
	// by a second statement.
	BackslashEscapes bool
	// NestedBlockComments makes /* /* */ */ a single comment. PostgreSQL nests;
	// MySQL ends the comment at the first */, exposing the rest as code.
	NestedBlockComments bool
	// ExecutableComments makes /*! ... */ bodies real SQL, as on MySQL.
	ExecutableComments bool
}

// dbq does not know which engine a statement is bound for at classification
// time — and an ODBC connection could be almost anything — so every statement
// is read under both profiles and the more dangerous reading wins.
var (
	dialectMySQL    = dialect{BackslashEscapes: true, NestedBlockComments: false, ExecutableComments: true}
	dialectStandard = dialect{BackslashEscapes: false, NestedBlockComments: true, ExecutableComments: false}
)

// lex turns SQL into tokens, discarding comments and literal contents.
//
// This is a lexer, not a parser. It exists purely so the classifier reads
// statement verbs from code rather than from data. It handles every comment and
// quoting form dbq's supported engines accept, because each unhandled form is a
// way to desynchronise the lexer and smuggle a verb past the gate:
//
//   - -- to end of line, # to end of line (MySQL)
//   - /* */ including PostgreSQL's nested block comments
//   - /*! ... */ and /*M! ... */ MySQL executable comments, whose bodies are
//     real SQL to the server and are therefore lexed inline rather than dropped
//   - '...' with both '' and backslash escapes
//   - "...", `...`, [...] quoted identifiers with doubled-quote escapes
//   - $$...$$ and $tag$...$tag$ PostgreSQL dollar quoting
//
// Unterminated constructs consume to end of input, which is what every engine
// does and which keeps the trailing text from being read as code.
func lex(sql string, d dialect) []token {
	l := &lexer{dialect: d}
	l.run([]rune(sql))

	return l.out
}

type lexer struct {
	dialect dialect
	out     []token
}

func (l *lexer) emit(t tokenType, text string) {
	l.out = append(l.out, token{Type: t, Text: text})
}

//nolint:gocognit,cyclop // A lexer is a dispatch table; splitting it would hide the ordering that matters.
func (l *lexer) run(src []rune) {
	for i := 0; i < len(src); {
		c := src[i]
		next := rune(0)

		if i+1 < len(src) {
			next = src[i+1]
		}

		switch {
		case c == '-' && next == '-':
			i = skipLine(src, i)

		case c == '#':
			i = skipLine(src, i)

		case c == '/' && next == '*':
			if l.dialect.ExecutableComments {
				if body, end, ok := executableComment(src, i); ok {
					// The server executes this body, so it must be classified.
					l.run(body)

					i = end

					continue
				}
			}

			i = skipBlockComment(src, i, l.dialect.NestedBlockComments)

		case c == '\'':
			end := skipQuoted(src, i, '\'', l.dialect.BackslashEscapes)

			// The content is kept only so that match-everything LIKE patterns
			// can be recognised. It is never treated as code.
			l.emit(tString, unquote(src[i+1:min(end-1, len(src))], '\''))

			i = end

		case c == '"' || c == '`':
			end := skipQuoted(src, i, c, false)

			l.emit(tQuoted, strings.ToLower(unquote(src[i+1:min(end-1, len(src))], c)))

			i = end

		case c == '[':
			end := skipQuoted(src, i, ']', false)

			l.emit(tQuoted, strings.ToLower(unquote(src[i+1:min(end-1, len(src))], ']')))

			i = end

		case c == '$':
			if tag, ok := dollarTag(src, i); ok {
				i = skipDollarQuoted(src, i, tag)

				l.emit(tString, "")

				continue
			}

			i = l.lexWord(src, i)

		case c == ';':
			l.emit(tSemi, ";")

			i++

		case c >= '0' && c <= '9':
			start := i
			for i < len(src) && (isDigit(src[i]) || src[i] == '.' || src[i] == 'e' || src[i] == 'E') {
				i++
			}

			l.emit(tNumber, string(src[start:i]))

		case isWordStart(c):
			i = l.lexWord(src, i)

		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++

		default:
			// Multi-character operators must stay whole so that <> and != are
			// not read as two separate tokens by the predicate analysis.
			if op, ok := operator(src, i); ok {
				l.emit(tPunct, op)

				i += len([]rune(op))

				continue
			}

			l.emit(tPunct, string(c))

			i++
		}
	}
}

func (l *lexer) lexWord(src []rune, i int) int {
	start := i
	for i < len(src) && isWordPart(src[i]) {
		i++
	}

	if i == start {
		l.emit(tPunct, string(src[i]))

		return i + 1
	}

	l.emit(tWord, strings.ToLower(string(src[start:i])))

	return i
}

func isWordStart(c rune) bool {
	return c == '_' || c == '$' || c == '@' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c > 127
}

func isWordPart(c rune) bool {
	return isWordStart(c) || isDigit(c)
}

func isDigit(c rune) bool {
	return c >= '0' && c <= '9'
}

var operators = []string{"<=>", "!=", "<>", ">=", "<=", "||", "::", ":="}

func operator(src []rune, i int) (string, bool) {
	rest := string(src[i:])
	for _, op := range operators {
		if strings.HasPrefix(rest, op) {
			return op, true
		}
	}

	return "", false
}

// skipLine consumes a line comment. Both \n and \r end it, so a lone-CR file
// cannot hide the rest of the statement inside a comment.
func skipLine(src []rune, i int) int {
	for i < len(src) && src[i] != '\n' && src[i] != '\r' {
		i++
	}

	return i
}

// skipBlockComment consumes /* ... */. When nested is false the comment ends at
// the first */, which is what MySQL does and which can expose the remainder of
// a `/* /* */ ; DROP TABLE t */` construct as code.
func skipBlockComment(src []rune, i int, nested bool) int {
	depth := 0

	for i < len(src) {
		switch {
		case i+1 < len(src) && src[i] == '/' && src[i+1] == '*':
			if !nested && depth > 0 {
				i += 2

				continue
			}

			depth++

			i += 2
		case i+1 < len(src) && src[i] == '*' && src[i+1] == '/':
			depth--

			i += 2

			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}

	return len(src)
}

// executableComment recognises MySQL/MariaDB conditional comments, whose bodies
// the server runs as ordinary SQL: /*! ... */, /*!50000 ... */, /*M! ... */.
// Dropping them like a normal comment would let `SELECT 1 /*! ,DROP TABLE t */`
// through as a plain read.
func executableComment(src []rune, start int) ([]rune, int, bool) {
	i := start + 2
	if i >= len(src) {
		return nil, 0, false
	}

	switch {
	case src[i] == '!':
		i++
	case src[i] == 'M' && i+1 < len(src) && src[i+1] == '!':
		i += 2
	default:
		return nil, 0, false
	}

	// An optional version gate such as 50000 in /*!50000 ... */.
	for i < len(src) && (isDigit(src[i]) || src[i] == ' ') {
		i++
	}

	body := i

	for i+1 < len(src) {
		if src[i] == '*' && src[i+1] == '/' {
			return src[body:i], i + 2, true
		}

		i++
	}

	return src[body:], len(src), true
}

// skipQuoted consumes a quoted run. escapes enables backslash escaping, which
// MySQL applies to string literals but never to identifiers.
func skipQuoted(src []rune, i int, closing rune, escapes bool) int {
	i++

	for i < len(src) {
		if escapes && src[i] == '\\' {
			i += 2

			continue
		}

		if src[i] == closing {
			// A doubled quote is an escaped quote, not the end.
			if i+1 < len(src) && src[i+1] == closing {
				i += 2

				continue
			}

			return i + 1
		}

		i++
	}

	return len(src)
}

func unquote(src []rune, closing rune) string {
	var b strings.Builder

	for i := 0; i < len(src); i++ {
		if src[i] == closing && i+1 < len(src) && src[i+1] == closing {
			b.WriteRune(closing)
			i++

			continue
		}

		b.WriteRune(src[i])
	}

	return b.String()
}

// dollarTag recognises the opening of a PostgreSQL dollar-quoted string,
// returning the full tag ("$$" or "$name$").
func dollarTag(src []rune, start int) (string, bool) {
	if src[start] != '$' {
		return "", false
	}

	i := start + 1
	if i < len(src) && src[i] == '$' {
		return "$$", true
	}

	if i >= len(src) || (src[i] != '_' && !isLetter(src[i])) {
		return "", false
	}

	for i < len(src) && (src[i] == '_' || isLetter(src[i]) || isDigit(src[i])) {
		i++
	}

	if i >= len(src) || src[i] != '$' {
		return "", false
	}

	return string(src[start : i+1]), true
}

func isLetter(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func skipDollarQuoted(src []rune, start int, tag string) int {
	runes := []rune(tag)
	i := start + len(runes)

	for i+len(runes) <= len(src) {
		if string(src[i:i+len(runes)]) == tag {
			return i + len(runes)
		}

		i++
	}

	return len(src)
}

// splitStatements splits a token stream on semicolons, dropping empty
// statements so that trailing and repeated `;` are not counted.
//
// Splitting after lexing rather than on the raw text is what makes
// `SELECT 'a; DROP TABLE t'` a single statement: the semicolon inside the
// literal never becomes a tSemi token.
func splitStatements(tokens []token) [][]token {
	var (
		out     [][]token
		current []token
	)

	for _, t := range tokens {
		if t.Type == tSemi {
			if len(current) > 0 {
				out = append(out, current)
				current = nil
			}

			continue
		}

		current = append(current, t)
	}

	if len(current) > 0 {
		out = append(out, current)
	}

	return out
}
