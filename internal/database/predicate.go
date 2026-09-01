package database

import "strings"

// predicateIsUnbounded reports whether a WHERE clause fails to limit the rows a
// statement touches, and why.
//
// This is a token-level approximation of what a real parser would prove. It is
// deliberately one-sided: it must never call a tautology "bounded", but it is
// allowed to call a genuinely narrow predicate "unbounded" and demand a higher
// permission level. Every rule below therefore errs towards refusing.
//
// The rules mirror the ones a mature implementation needs:
//
//  1. a predicate that names no column cannot depend on the data, so it is the
//     same for every row: WHERE 1=1, WHERE TRUE, WHERE 2>1, WHERE abs(1)=1
//  2. a subquery makes the row count unknowable statically
//  3. self-comparison: WHERE id = id
//  4. match-everything patterns: WHERE name LIKE '%'
//  5. complementary branches across a top-level OR: WHERE id = 1 OR id <> 1,
//     WHERE id IS NULL OR id IS NOT NULL
func predicateIsUnbounded(ts []token) (string, bool) {
	ts = trimTrailingClauses(ts)

	if len(strip(ts)) == 0 {
		return "has an empty WHERE clause", true
	}

	// A subquery anywhere in the predicate makes the affected row count
	// impossible to bound statically, so it is refused outright.
	if containsSubquery(ts) {
		return "has a WHERE clause containing a subquery, whose reach cannot be determined", true
	}

	if reason, unbounded := exprIsUnbounded(ts); unbounded {
		return reason, true
	}

	return "", false
}

// exprIsUnbounded decides whether a boolean expression matches every row.
//
// The recursion follows the logic of the connectives:
//
//   - OR is unbounded if ANY branch is unbounded, or if two branches are
//     complementary and so cover everything between them.
//   - AND is unbounded only if EVERY branch is unbounded, because a single
//     selective conjunct narrows the whole predicate.
//   - a leaf is unbounded if it names no column, compares a value with itself,
//     or is a match-everything pattern.
//
// Where the analysis cannot decide, it reports unbounded. The cost of that is a
// statement being pushed up to the `full` permission level; the cost of the
// opposite error is an unintended table-wide write.
func exprIsUnbounded(ts []token) (string, bool) {
	ts = strip(ts)
	if len(ts) == 0 {
		return "has an empty predicate", true
	}

	if branches := splitTopLevel(ts, "or"); len(branches) > 1 {
		for _, branch := range branches {
			if reason, unbounded := exprIsUnbounded(branch); unbounded {
				return reason, true
			}
		}

		for i := range branches {
			for j := i + 1; j < len(branches); j++ {
				if branchesAreComplementary(branches[i], branches[j]) {
					return "has an OR predicate whose branches together cover every row", true
				}
			}
		}

		// Proving that `a OR (b AND c)` is not a tautology needs real boolean
		// reasoning, which this analysis does not attempt.
		for _, branch := range branches {
			if len(splitTopLevel(branch, "and")) > 1 {
				return "has an OR predicate mixing AND branches, which cannot be shown to be selective", true
			}
		}

		return "", false
	}

	if branches := splitTopLevel(ts, "and"); len(branches) > 1 {
		var reason string

		for _, branch := range branches {
			r, unbounded := exprIsUnbounded(branch)
			if !unbounded {
				return "", false
			}

			reason = r
		}

		return reason, true
	}

	return leafIsUnbounded(ts)
}

func leafIsUnbounded(ts []token) (string, bool) {
	if !containsColumnReference(ts) {
		return "has a WHERE clause that references no column, so it matches every row", true
	}

	if isSelfComparison(ts) {
		return "has a WHERE clause comparing a value with itself, which matches every row", true
	}

	if matchesEverythingLike(ts) {
		return "has a LIKE pattern that matches every row", true
	}

	return "", false
}

// trimTrailingClauses drops clauses that follow the predicate so they are not
// mistaken for part of it. Note that LIMIT is deliberately not treated as
// bounding: `DELETE FROM t LIMIT 10` still picks an arbitrary ten rows.
func trimTrailingClauses(ts []token) []token {
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

		if depth == 0 && ts[i].Type == tWord && trailingClauseKeywords[ts[i].Text] {
			return ts[:i]
		}
	}

	return ts
}

var trailingClauseKeywords = map[string]bool{
	"returning": true, "order": true, "limit": true, "offset": true, "group": true,
}

func containsSubquery(ts []token) bool {
	for i := 1; i < len(ts); i++ {
		if ts[i].is("select") {
			return true
		}
	}

	return false
}

// containsColumnReference reports whether any token could name a column.
//
// A bare word only counts when it is not a keyword, not a niladic value
// function such as CURRENT_USER, and not the name of a function being called.
// Excluding the function-call case is what makes `WHERE abs(1) = 1` unbounded
// while `WHERE abs(id) = 1` stays bounded.
func containsColumnReference(ts []token) bool {
	for i := range ts {
		if ts[i].Type == tQuoted {
			return true
		}

		if ts[i].Type != tWord {
			continue
		}

		word := ts[i].Text
		if predicateKeywords[word] || valueKeywords[word] {
			continue
		}

		// `name(` is a call, not a column.
		if i+1 < len(ts) && ts[i+1].Type == tPunct && ts[i+1].Text == "(" {
			continue
		}

		return true
	}

	return false
}

// predicateKeywords are words that may appear in a predicate without naming a
// column.
var predicateKeywords = map[string]bool{
	"and": true, "or": true, "not": true, "is": true, "null": true,
	"true": true, "false": true, "unknown": true, "in": true, "between": true,
	"like": true, "ilike": true, "rlike": true, "regexp": true, "similar": true,
	"to": true, "escape": true, "any": true, "all": true, "some": true,
	"exists": true, "case": true, "when": true, "then": true, "else": true,
	"end": true, "cast": true, "as": true, "distinct": true, "from": true,
	"interval": true, "date": true, "time": true, "timestamp": true,
	"year": true, "month": true, "day": true, "hour": true, "minute": true,
	"second": true, "extract": true, "collate": true, "binary": true,
}

// valueKeywords are niladic functions that produce a value without reading a
// column, so they cannot narrow a predicate.
var valueKeywords = map[string]bool{
	"current_catalog": true, "current_date": true, "current_role": true,
	"current_schema": true, "current_time": true, "current_timestamp": true,
	"current_user": true, "localtime": true, "localtimestamp": true,
	"session_user": true, "system_user": true, "user": true, "sysdate": true,
}

// isSelfComparison detects `X = X` where both sides are the same token run.
func isSelfComparison(ts []token) bool {
	ops := map[string]bool{"=": true, ">=": true, "<=": true, "<=>": true}

	for i := range ts {
		if ts[i].Type != tPunct || !ops[ts[i].Text] {
			continue
		}

		left := strip(ts[:i])
		right := strip(ts[i+1:])

		if len(left) > 0 && tokensEqual(left, right) {
			return true
		}
	}

	return false
}

// matchesEverythingLike detects LIKE / ILIKE against a pattern of only '%'.
func matchesEverythingLike(ts []token) bool {
	for i := range ts {
		if !ts[i].is("like") && !ts[i].is("ilike") {
			continue
		}

		if i+1 < len(ts) && ts[i+1].Type == tString && isAllPercent(ts[i+1].Text) {
			return true
		}
	}

	return false
}

func isAllPercent(s string) bool {
	if s == "" {
		return false
	}

	return strings.Trim(s, "%") == ""
}

// splitTopLevel splits a token run on a keyword appearing at paren depth 0.
func splitTopLevel(ts []token, word string) [][]token {
	var (
		out   [][]token
		start int
		depth int
	)

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

		if depth == 0 && ts[i].is(word) {
			out = append(out, strip(ts[start:i]))
			start = i + 1
		}
	}

	out = append(out, strip(ts[start:]))

	return out
}

// branchesAreComplementary reports whether two OR branches together match every
// row: `x IS NULL` / `x IS NOT NULL`, or `x = v` / `x <> v`.
func branchesAreComplementary(a, b []token) bool {
	if subject, negA, ok := nullCheck(a); ok {
		if other, negB, ok2 := nullCheck(b); ok2 && negA != negB && tokensEqual(subject, other) {
			return true
		}
	}

	leftA, opA, rightA, okA := comparison(a)
	leftB, opB, rightB, okB := comparison(b)

	if !okA || !okB {
		return false
	}

	// Normalise `1 = id` to `id = 1` so mirrored branches still pair up.
	if !tokensEqual(leftA, leftB) {
		leftB, rightB, opB = rightB, leftB, reverseOp(opB)
	}

	return tokensEqual(leftA, leftB) && tokensEqual(rightA, rightB) && complementaryOps[[2]string{opA, opB}]
}

var complementaryOps = map[[2]string]bool{
	{"=", "<>"}: true, {"<>", "="}: true,
	{"=", "!="}: true, {"!=", "="}: true,
	{">", "<="}: true, {"<=", ">"}: true,
	{"<", ">="}: true, {">=", "<"}: true,
}

func reverseOp(op string) string {
	switch op {
	case ">":
		return "<"
	case "<":
		return ">"
	case ">=":
		return "<="
	case "<=":
		return ">="
	default:
		return op
	}
}

// nullCheck matches `X IS [NOT] NULL`, returning the subject and whether the
// check is negated. A leading NOT flips the sense.
func nullCheck(ts []token) ([]token, bool, bool) {
	ts = strip(ts)

	negated := false

	for len(ts) > 0 && ts[0].is("not") {
		negated = !negated
		ts = strip(ts[1:])
	}

	for i := range ts {
		if !ts[i].is("is") {
			continue
		}

		rest := ts[i+1:]
		if len(rest) > 0 && rest[0].is("not") {
			negated = !negated
			rest = rest[1:]
		}

		if len(rest) == 1 && rest[0].is("null") {
			return strip(ts[:i]), negated, true
		}

		return nil, false, false
	}

	return nil, false, false
}

// comparison matches `X <op> Y` at depth 0.
func comparison(ts []token) ([]token, string, []token, bool) {
	ts = strip(ts)

	negated := false
	for len(ts) > 0 && ts[0].is("not") {
		negated = !negated
		ts = strip(ts[1:])
	}

	ops := map[string]bool{"=": true, "<>": true, "!=": true, ">": true, "<": true, ">=": true, "<=": true}
	depth := 0

	for i := range ts {
		if ts[i].Type != tPunct {
			continue
		}

		switch ts[i].Text {
		case "(":
			depth++

			continue
		case ")":
			depth--

			continue
		}

		if depth != 0 || !ops[ts[i].Text] {
			continue
		}

		op := ts[i].Text
		if negated {
			op = negateOp(op)
		}

		return strip(ts[:i]), op, strip(ts[i+1:]), true
	}

	return nil, "", nil, false
}

func negateOp(op string) string {
	switch op {
	case "=":
		return "<>"
	case "<>", "!=":
		return "="
	case ">":
		return "<="
	case "<=":
		return ">"
	case "<":
		return ">="
	case ">=":
		return "<"
	default:
		return op
	}
}

// strip removes surrounding whitespace-equivalent noise: redundant outer
// parentheses, so `((id))` compares equal to `id`.
func strip(ts []token) []token {
	for len(ts) >= 2 &&
		ts[0].Type == tPunct && ts[0].Text == "(" &&
		ts[len(ts)-1].Type == tPunct && ts[len(ts)-1].Text == ")" &&
		balanced(ts[1:len(ts)-1]) {
		ts = ts[1 : len(ts)-1]
	}

	return ts
}

func balanced(ts []token) bool {
	depth := 0

	for i := range ts {
		if ts[i].Type != tPunct {
			continue
		}

		switch ts[i].Text {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return false
			}
		}
	}

	return depth == 0
}

func tokensEqual(a, b []token) bool {
	a, b = strip(a), strip(b)
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].Type != b[i].Type || a[i].Text != b[i].Text {
			return false
		}
	}

	return true
}
