package database

import (
	"maps"
	"slices"
	"strings"
)

// SplitStatements cuts a script into statements at top-level semicolons.
//
// It runs the same lexer the classifier uses, so a semicolon inside a string,
// a comment or a dollar-quoted block does not end a statement. Each returned
// statement keeps its terminating semicolon when it had one; blank pieces are
// dropped.
//
// A semicolon counts when it is top-level under either lexical dialect. The
// splitter does not know which engine the text is bound for, and requiring
// agreement would leave a MySQL user's `SELECT 'it\'s';` waiting forever for
// a terminator. The classifier still refuses anything that reads as a batch
// under either dialect, so the choice affects only where input is cut.
func SplitStatements(sql string) []string {
	complete, rest := SplitComplete(sql)

	return appendStatement(complete, []rune(rest))
}

// SplitComplete returns the statements terminated by a top-level semicolon and
// the unterminated text after the last one, verbatim so a caller can keep
// buffering it. It is what an interactive reader needs: run what is complete,
// keep the rest.
func SplitComplete(sql string) ([]string, string) {
	src := []rune(sql)

	var (
		out   []string
		start int
	)

	for _, end := range statementEnds(src) {
		out = appendStatement(out, src[start:end])
		start = end
	}

	return out, string(src[start:])
}

// appendStatement adds the trimmed piece unless it holds no code: blank, a
// bare `;`, or only comments, which a script routinely ends with.
func appendStatement(out []string, piece []rune) []string {
	text := strings.TrimSpace(string(piece))
	if len(splitStatements(lex(text, dialectStandard))) == 0 {
		return out
	}

	return append(out, text)
}

// statementEnds returns the rune offsets just past each semicolon that is
// top-level under at least one dialect, in ascending order.
func statementEnds(src []rune) []int {
	ends := map[int]struct{}{}

	for _, d := range []dialect{dialectStandard, dialectMySQL} {
		l := &lexer{dialect: d, trackSemis: true}
		l.run(src)

		for _, end := range l.semis {
			ends[end] = struct{}{}
		}
	}

	return slices.Sorted(maps.Keys(ends))
}
