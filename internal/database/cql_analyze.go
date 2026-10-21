package database

import (
	"fmt"
	"strings"
)

// AnalyzeCQL classifies a Cassandra CQL statement.
//
// CQL is close enough to SQL that the SQL lexer, in its CQL profile, and the
// SQL predicate analysis carry over. What differs is the statement set: there
// are no joins or subqueries, INSERT and UPDATE are upserts of the rows they
// name, and BEGIN ... APPLY BATCH wraps several writes in one statement.
func AnalyzeCQL(statement string) Analysis {
	statements := splitStatements(lex(statement, dialectCQL))

	if len(statements) == 0 {
		return Analysis{Kind: KindUnknown, Refused: true, Reason: "no CQL statement found"}
	}

	if statements[0][0].is("begin") && isCQLBatch(statements[0]) {
		return analyzeCQLBatch(statements)
	}

	if len(statements) > 1 {
		return Analysis{
			Kind:    KindUnknown,
			Refused: true,
			Reason:  fmt.Sprintf("%d statements were submitted in one call; send exactly one statement", len(statements)),
		}
	}

	return analyzeCQLStatement(statements[0])
}

func analyzeCQLStatement(ts []token) Analysis {
	head := ts[0]
	if head.Type != tWord {
		return Analysis{Kind: KindUnknown, Unbounded: true, Reason: "statement could not be classified"}
	}

	switch head.Text {
	case "select", "describe", "desc":
		return Analysis{Kind: KindRead}
	case "list":
		// LIST ROLES / USERS / PERMISSIONS only report.
		return Analysis{Kind: KindRead}
	case "insert":
		// A CQL INSERT writes exactly the row its primary key names, even
		// when that row exists, so its reach is always one row.
		return Analysis{Kind: KindWrite}
	case "update", "delete":
		return analyzeCQLUpdateDelete(head.Text, ts)
	case "use", "begin":
		return Analysis{
			Kind:    KindSession,
			Refused: true,
			Reason:  "USE changes the keyspace of a pooled connection, which would leak into unrelated queries",
			Advice:  "Qualify table names as keyspace.table instead",
		}
	}

	if kind, known := verbKind(head.Text); known && kind == KindSchema {
		return Analysis{Kind: KindSchema}
	}

	// GRANT, REVOKE and the role statements are covered by verbKind; anything
	// else is unknown and needs full access.
	return Analysis{Kind: KindUnknown, Unbounded: true, Reason: "statement could not be classified"}
}

// analyzeCQLUpdateDelete requires a WHERE clause that is not a tautology.
// Cassandra itself insists on the partition key, so in practice this mostly
// catches statements the server would reject anyway, but the gate must not
// depend on that.
func analyzeCQLUpdateDelete(verb string, ts []token) Analysis {
	upper := strings.ToUpper(verb)
	whereAt := -1

	for i := range ts {
		if ts[i].is("where") {
			whereAt = i

			break
		}
	}

	if whereAt < 0 {
		return Analysis{Kind: KindWrite, Unbounded: true, Reason: upper + " without a WHERE clause"}
	}

	// A lightweight-transaction condition follows the predicate and only
	// narrows it further; it is not part of the row selection.
	predicate := ts[whereAt+1:]
	for i := range predicate {
		if predicate[i].is("if") {
			predicate = predicate[:i]

			break
		}
	}

	if reason, unbounded := predicateIsUnbounded(predicate); unbounded {
		return Analysis{Kind: KindWrite, Unbounded: true, Reason: upper + " " + reason}
	}

	return Analysis{Kind: KindWrite}
}

// isCQLBatch reports whether a statement opens a batch:
// BEGIN [UNLOGGED | COUNTER | LOGGED] BATCH.
func isCQLBatch(ts []token) bool {
	for i := 1; i < len(ts) && i <= 2; i++ {
		if ts[i].is("batch") {
			return true
		}
	}

	return false
}

// batchHeaderToken reports whether t may appear in
// BEGIN [UNLOGGED | COUNTER | LOGGED] BATCH [USING TIMESTAMP n].
func batchHeaderToken(t token) bool {
	if t.Type == tNumber {
		return true
	}

	switch {
	case t.is("begin"), t.is("unlogged"), t.is("counter"), t.is("logged"),
		t.is("batch"), t.is("using"), t.is("timestamp"):
		return true
	default:
		return false
	}
}

// analyzeCQLBatch classifies BEGIN BATCH ... APPLY BATCH, whose inner writes
// are separated by semicolons and so arrive as several statements. Each inner
// statement must be an INSERT, UPDATE or DELETE; the batch is as dangerous as
// its most dangerous member.
func analyzeCQLBatch(statements [][]token) Analysis {
	last := statements[len(statements)-1]
	n := len(last)

	if n < 2 || !last[n-2].is("apply") || !last[n-1].is("batch") {
		return Analysis{
			Kind: KindUnknown, Refused: true,
			Reason: "BEGIN BATCH must end with APPLY BATCH, and nothing may follow it",
		}
	}

	// The first piece carries the BEGIN ... BATCH [USING ...] header in
	// front of the first write; the last carries APPLY BATCH after the last.
	pieces := make([][]token, len(statements))
	copy(pieces, statements)

	head := pieces[0]
	start := -1

	for i := range head {
		if head[i].Type == tWord && writeVerbs[head[i].Text] {
			start = i

			break
		}

		// Only the batch header may precede the first write; anything else
		// would be a statement the loop below never sees.
		if !batchHeaderToken(head[i]) {
			return Analysis{Kind: KindUnknown, Refused: true, Reason: "unexpected text in the BEGIN BATCH header"}
		}
	}

	if start < 0 {
		return Analysis{Kind: KindUnknown, Refused: true, Reason: "BEGIN BATCH contains no write"}
	}

	pieces[0] = head[start:]
	pieces[len(pieces)-1] = pieces[len(pieces)-1][:len(pieces[len(pieces)-1])-2]

	result := Analysis{Kind: KindWrite}

	for _, piece := range pieces {
		if len(piece) == 0 {
			continue
		}

		if piece[0].Type != tWord || (piece[0].Text != "insert" && piece[0].Text != "update" && piece[0].Text != "delete") {
			return Analysis{
				Kind: KindUnknown, Refused: true,
				Reason: "a batch may only contain INSERT, UPDATE and DELETE statements",
			}
		}

		for i := 1; i < len(piece); i++ {
			if piece[i-1].is("apply") && piece[i].is("batch") {
				return Analysis{Kind: KindUnknown, Refused: true, Reason: "APPLY BATCH may only end the batch"}
			}
		}

		if a := analyzeCQLStatement(piece); a.Unbounded {
			result.Unbounded = true
			result.Reason = "batch member: " + a.Reason
		}
	}

	return result
}
