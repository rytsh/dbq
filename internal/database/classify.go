package database

import "fmt"

// Kind is the coarse classification of a SQL statement.
type Kind string

const (
	// KindRead only reads: SELECT, SHOW, EXPLAIN, WITH ... SELECT.
	KindRead Kind = "read"
	// KindWrite modifies data: INSERT, UPDATE, DELETE, MERGE.
	KindWrite Kind = "write"
	// KindSchema modifies schema or privileges, or can touch the filesystem:
	// CREATE, DROP, ALTER, TRUNCATE, GRANT, COPY, SELECT ... INTO OUTFILE.
	KindSchema Kind = "schema"
	// KindSession mutates connection-local state: BEGIN, COMMIT, USE, SET.
	// Always refused, at every permission level — see Analysis.
	KindSession Kind = "session"
	// KindUnknown is a statement dbq could not classify. Requires full access.
	KindUnknown Kind = "unknown"
)

// rank orders the kinds so a compound statement can take the maximum.
var kindRank = map[Kind]int{
	KindRead:    0,
	KindWrite:   1,
	KindSchema:  2,
	KindUnknown: 3,
	KindSession: 4,
}

func maxKind(a, b Kind) Kind {
	if kindRank[b] > kindRank[a] {
		return b
	}

	return a
}

// ReturnsRows reports whether a statement of this kind produces a result set,
// which decides between Query and Exec.
func (k Kind) ReturnsRows() bool {
	return k == KindRead
}

// Analysis is the result of inspecting one SQL statement.
type Analysis struct {
	// Kind is the statement category.
	Kind Kind
	// Unbounded marks a data-modifying statement whose blast radius cannot be
	// shown to be limited: no WHERE clause, a predicate that is really a
	// tautology, a multi-table target, or an INSERT that is not plain VALUES.
	Unbounded bool
	// Refused marks a statement that is rejected at every permission level,
	// because no permission level can make it safe to run here.
	Refused bool
	// Reason explains Unbounded or Refused, in terms an operator and a model
	// can both act on.
	Reason string
}

// severity orders analyses so the most dangerous reading of an ambiguous
// statement can be selected.
func (a Analysis) severity() int {
	if a.Refused {
		return 1000
	}

	s := a.RequiredPermission().Rank() * 2
	if a.Unbounded {
		s++
	}

	return s
}

// RequiredPermission is the minimum level needed to run the statement.
//
// The level depends on blast radius, not only on category: a bounded
// `DELETE ... WHERE id = 1` needs safe-write, but `DELETE FROM users` with no
// effective filter needs full. Without that distinction "safe-write" would
// permit emptying every table, which is not what the name promises.
func (a Analysis) RequiredPermission() Permission {
	switch a.Kind {
	case KindRead:
		return PermissionReadOnly
	case KindWrite:
		if a.Unbounded {
			return PermissionFull
		}

		return PermissionSafeWrite
	case KindSchema, KindSession, KindUnknown:
		return PermissionFull
	default:
		return PermissionFull
	}
}

var (
	readVerbs = map[string]bool{
		"select": true, "show": true, "describe": true, "desc": true,
		"values": true, "table": true, "pragma": true, "explain": true,
	}
	writeVerbs = map[string]bool{
		"insert": true, "update": true, "delete": true, "merge": true,
		"upsert": true, "replace": true,
	}
	// Schema-and-beyond: DDL, privileges, maintenance, and anything that can
	// reach the filesystem or run code.
	schemaVerbs = map[string]bool{
		"create": true, "drop": true, "alter": true, "truncate": true,
		"rename": true, "grant": true, "revoke": true, "comment": true,
		"vacuum": true, "analyze": true, "reindex": true, "cluster": true,
		"refresh": true, "attach": true, "detach": true, "call": true,
		"do": true, "exec": true, "execute": true, "copy": true, "load": true,
		"import": true, "export": true, "backup": true, "restore": true,
		"shutdown": true, "kill": true, "install": true, "checkpoint": true,
	}
	// Statements that change state on the connection itself. dbq hands out
	// pooled connections, so these would leak into an unrelated caller's next
	// query; they are refused rather than gated.
	sessionVerbs = map[string]bool{
		"begin": true, "start": true, "commit": true, "rollback": true,
		"savepoint": true, "release": true, "end": true, "use": true,
		"set": true, "reset": true, "discard": true, "lock": true,
		"unlock": true, "prepare": true, "deallocate": true, "declare": true,
		"fetch": true, "close": true, "move": true, "listen": true,
		"unlisten": true, "notify": true,
	}
)

func verbKind(word string) (Kind, bool) {
	switch {
	case readVerbs[word]:
		return KindRead, true
	case writeVerbs[word]:
		return KindWrite, true
	case schemaVerbs[word]:
		return KindSchema, true
	case sessionVerbs[word]:
		return KindSession, true
	default:
		return KindUnknown, false
	}
}

// Analyze classifies a SQL statement and measures its blast radius.
//
// The statement is read under every lexical dialect dbq supports and the most
// dangerous reading wins. That matters because the same text can be a single
// harmless read on one engine and a two-statement batch ending in DROP TABLE on
// another — `SELECT 'a\'; DROP TABLE users; --'` is one string literal to MySQL
// and two statements to PostgreSQL. Choosing a dialect from the connection type
// would make a misconfigured driver name into a bypass, so both are checked.
func Analyze(sql string) Analysis {
	worst := analyzeDialect(sql, dialectMySQL)

	if other := analyzeDialect(sql, dialectStandard); other.severity() > worst.severity() {
		worst = other
	}

	return worst
}

func analyzeDialect(sql string, d dialect) Analysis {
	statements := splitStatements(lex(sql, d))

	switch len(statements) {
	case 0:
		return Analysis{Kind: KindUnknown, Refused: true, Reason: "no SQL statement found"}
	case 1:
		return analyzeStatement(statements[0])
	default:
		// Classifying only the leading verb of a batch would let
		// `SELECT 1; DROP TABLE users` pass as a read, and whether the trailing
		// statement actually executes would then depend on driver settings.
		// A security boundary cannot rest on that, so batches are refused.
		return Analysis{
			Kind:    KindUnknown,
			Refused: true,
			Reason: fmt.Sprintf(
				"%d statements were submitted in one call; send exactly one statement", len(statements),
			),
		}
	}
}

// Classify reports only the category of a statement.
func Classify(sql string) Kind {
	return Analyze(sql).Kind
}

func analyzeStatement(ts []token) Analysis {
	if len(ts) == 0 {
		return Analysis{Kind: KindUnknown, Reason: "empty statement"}
	}

	head := ts[0]
	if !head.is("explain") {
		return analyzeVerb(ts)
	}

	// EXPLAIN ANALYZE actually executes the statement, and even plain EXPLAIN
	// executes the inner statement on some engines. Inherit the inner kind
	// rather than reporting a read.
	inner := stripExplain(ts)
	if len(inner) == 0 {
		return Analysis{Kind: KindRead}
	}

	return analyzeVerb(inner)
}

// stripExplain removes EXPLAIN and its options, returning the inner statement.
func stripExplain(ts []token) []token {
	i := 1

	for i < len(ts) {
		switch {
		case ts[i].Type == tWord && explainOptions[ts[i].Text]:
			i++
		case ts[i].Type == tPunct && ts[i].Text == "(":
			depth := 0

			for i < len(ts) {
				if ts[i].Type == tPunct && ts[i].Text == "(" {
					depth++
				}

				if ts[i].Type == tPunct && ts[i].Text == ")" {
					depth--
					i++

					if depth == 0 {
						break
					}

					continue
				}

				i++
			}
		default:
			return ts[i:]
		}
	}

	return nil
}

var explainOptions = map[string]bool{
	"analyze": true, "analyse": true, "verbose": true, "costs": true,
	"buffers": true, "timing": true, "summary": true, "format": true,
	"plan": true, "for": true, "extended": true, "partitions": true,
}

func analyzeVerb(ts []token) Analysis {
	head := ts[0]

	if head.is("with") {
		return analyzeCTE(ts)
	}

	kind, known := verbKind(head.Text)
	if head.Type != tWord || !known {
		return Analysis{
			Kind:      KindUnknown,
			Unbounded: true,
			Reason:    "statement could not be classified",
		}
	}

	switch kind {
	case KindRead:
		return analyzeRead(ts)
	case KindWrite:
		return analyzeWrite(head.Text, ts)
	case KindSession:
		return Analysis{
			Kind:    KindSession,
			Refused: true,
			Reason: "connection-state statements (BEGIN/COMMIT/USE/SET/LOCK) are not supported: " +
				"dbq runs each statement on a pooled connection, so the change would leak into unrelated queries",
		}
	case KindSchema, KindUnknown:
		return Analysis{Kind: kind}
	default:
		return Analysis{Kind: kind}
	}
}

// analyzeCTE resolves `WITH ... ` to the kind of the work it really performs.
//
// A CTE body may itself be a write on PostgreSQL
// (`WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x`), so the whole token
// stream is scanned for a verb in statement position, at any nesting depth,
// rather than only the top level after the CTE list.
func analyzeCTE(ts []token) Analysis {
	result := Analysis{Kind: KindRead}

	for i := 1; i < len(ts); i++ {
		if ts[i].Type != tWord {
			continue
		}

		// Statement position: the start of a parenthesised body, or top level.
		prev := ts[i-1]
		atStart := prev.Type == tPunct && (prev.Text == "(" || prev.Text == ")" || prev.Text == ",")

		if !atStart {
			continue
		}

		kind, known := verbKind(ts[i].Text)
		if !known || kind == KindRead {
			continue
		}

		if kind == KindWrite {
			sub := analyzeWrite(ts[i].Text, ts[i:])
			result.Kind = maxKind(result.Kind, KindWrite)

			if sub.Unbounded {
				result.Unbounded = true
				result.Reason = sub.Reason
			}

			continue
		}

		result.Kind = maxKind(result.Kind, kind)
	}

	if result.Kind == KindRead {
		return analyzeRead(ts)
	}

	if result.Kind == KindSession {
		return Analysis{Kind: KindSession, Refused: true, Reason: "connection-state statement inside a CTE"}
	}

	return result
}

// analyzeRead re-examines a nominally read-only statement for clauses that make
// it write, lock, or touch the filesystem.
func analyzeRead(ts []token) Analysis {
	depth := 0

	for i := range ts {
		if ts[i].Type == tPunct {
			switch ts[i].Text {
			case "(":
				depth++
			case ")":
				depth--
			}

			continue
		}

		if ts[i].Type != tWord {
			continue
		}

		switch ts[i].Text {
		case "for":
			// SELECT ... FOR UPDATE / FOR SHARE takes row locks and blocks
			// other writers, so it is not a read for permission purposes.
			if next, ok := wordAt(ts, i+1); ok && lockingModes[next] {
				return Analysis{Kind: KindWrite, Reason: "locking read (FOR " + next + ")"}
			}

		case "into":
			if depth != 0 {
				continue
			}

			next, ok := wordAt(ts, i+1)
			if ok && (next == "outfile" || next == "dumpfile") {
				return Analysis{
					Kind:   KindSchema,
					Reason: "SELECT ... INTO " + next + " writes to the database server's filesystem",
				}
			}

			// SELECT ... INTO newtable creates a table on PostgreSQL and
			// SQL Server.
			if ok {
				return Analysis{Kind: KindSchema, Reason: "SELECT ... INTO creates a new table"}
			}
		}
	}

	return Analysis{Kind: KindRead}
}

var lockingModes = map[string]bool{"update": true, "share": true, "key": true, "no": true}

func wordAt(ts []token, i int) (string, bool) {
	if i < 0 || i >= len(ts) || ts[i].Type != tWord {
		return "", false
	}

	return ts[i].Text, true
}

// analyzeWrite measures the blast radius of a data-modifying statement.
func analyzeWrite(verb string, ts []token) Analysis {
	switch verb {
	case "merge", "upsert":
		// MERGE combines insert, update and delete under one predicate; it is
		// never treatable as bounded.
		return Analysis{Kind: KindWrite, Unbounded: true, Reason: "MERGE can modify arbitrarily many rows"}
	case "replace":
		return Analysis{Kind: KindWrite, Unbounded: true, Reason: "REPLACE overwrites existing rows"}
	case "insert":
		return analyzeInsert(ts)
	case "update", "delete":
		return analyzeUpdateDelete(verb, ts)
	default:
		return Analysis{Kind: KindWrite, Unbounded: true, Reason: "unrecognised write statement"}
	}
}

// analyzeInsert treats only a plain INSERT ... VALUES as bounded. An
// INSERT ... SELECT inserts however many rows the query returns, and a conflict
// clause turns the statement into an update of unknown reach.
func analyzeInsert(ts []token) Analysis {
	depth := 0

	for i := range ts {
		if ts[i].Type == tPunct {
			switch ts[i].Text {
			case "(":
				depth++
			case ")":
				depth--
			}

			continue
		}

		if ts[i].Type != tWord || depth != 0 {
			continue
		}

		switch ts[i].Text {
		case "select", "with":
			return Analysis{Kind: KindWrite, Unbounded: true, Reason: "INSERT ... SELECT inserts an unbounded number of rows"}
		case "overwrite":
			return Analysis{Kind: KindWrite, Unbounded: true, Reason: "INSERT OVERWRITE replaces existing data"}
		case "duplicate":
			return Analysis{Kind: KindWrite, Unbounded: true, Reason: "ON DUPLICATE KEY UPDATE modifies existing rows"}
		case "conflict":
			// ON CONFLICT DO NOTHING is bounded; DO UPDATE is not.
			if hasWordAfter(ts[i:], "update") {
				return Analysis{Kind: KindWrite, Unbounded: true, Reason: "ON CONFLICT DO UPDATE modifies existing rows"}
			}
		case "replace":
			return Analysis{Kind: KindWrite, Unbounded: true, Reason: "INSERT OR REPLACE overwrites existing rows"}
		}
	}

	return Analysis{Kind: KindWrite}
}

func hasWordAfter(ts []token, word string) bool {
	for i := range ts {
		if ts[i].is(word) {
			return true
		}
	}

	return false
}

// analyzeUpdateDelete requires a single target table and an effective predicate.
func analyzeUpdateDelete(verb string, ts []token) Analysis {
	upper := "UPDATE"
	if verb == "delete" {
		upper = "DELETE"
	}

	depth := 0
	whereAt := -1

	for i := range ts {
		if ts[i].Type == tPunct {
			switch ts[i].Text {
			case "(":
				depth++
			case ")":
				depth--
			}

			continue
		}

		if ts[i].Type != tWord || depth != 0 {
			continue
		}

		switch ts[i].Text {
		case "join", "using":
			// A joined UPDATE/DELETE spans more than one table, so a predicate
			// on one of them says nothing about the rows touched in the other.
			return Analysis{
				Kind: KindWrite, Unbounded: true,
				Reason: upper + " across joined tables cannot be shown to be bounded",
			}
		case "from":
			// UPDATE ... FROM is the PostgreSQL join form. For DELETE, FROM is
			// the ordinary target clause.
			if verb == "update" {
				return Analysis{
					Kind: KindWrite, Unbounded: true,
					Reason: "UPDATE ... FROM joins another table and cannot be shown to be bounded",
				}
			}
		case "where":
			whereAt = i
		}
	}

	if whereAt < 0 {
		return Analysis{
			Kind: KindWrite, Unbounded: true,
			Reason: upper + " without a WHERE clause affects every row in the table",
		}
	}

	if reason, unbounded := predicateIsUnbounded(ts[whereAt+1:]); unbounded {
		return Analysis{Kind: KindWrite, Unbounded: true, Reason: upper + " " + reason}
	}

	return Analysis{Kind: KindWrite}
}
