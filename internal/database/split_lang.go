package database

import (
	"strings"
)

// Splitter cuts buffered input into complete statements and the unfinished
// remainder, which the REPL keeps buffering.
type Splitter func(buffer string) (complete []string, rest string)

// SplitterFor returns how statements are delimited in a query language.
func SplitterFor(language string) Splitter {
	switch language {
	case LanguageCQL:
		return splitCQL
	case LanguageMongoDB:
		return splitMongo
	case LanguageRedis:
		return splitRedis
	default:
		return SplitComplete
	}
}

// splitCQL splits at top-level semicolons like SQL, except that a
// BEGIN ... BATCH is kept whole up to its APPLY BATCH.
func splitCQL(buffer string) ([]string, string) {
	src := []rune(buffer)

	l := &lexer{dialect: dialectCQL, trackSemis: true}
	l.run(src)

	var (
		out          []string
		start, batch int
	)

	batch = -1

	for _, end := range l.semis {
		piece := strings.TrimSpace(string(src[start:end]))
		tokens := splitStatements(lex(piece, dialectCQL))

		if batch < 0 && len(tokens) > 0 && tokens[0][0].is("begin") && isCQLBatch(tokens[0]) {
			batch = start
		}

		if batch >= 0 {
			if !endsWithApplyBatch(tokens) {
				start = end

				continue
			}

			piece = strings.TrimSpace(string(src[batch:end]))
			batch = -1
		}

		if len(tokens) > 0 {
			out = append(out, piece)
		}

		start = end
	}

	if batch >= 0 {
		start = batch
	}

	return out, string(src[start:])
}

func endsWithApplyBatch(statements [][]token) bool {
	if len(statements) == 0 {
		return false
	}

	last := statements[len(statements)-1]
	n := len(last)

	return n >= 2 && last[n-2].is("apply") && last[n-1].is("batch")
}

// splitRedis takes one command per line, as redis-cli does.
func splitRedis(buffer string) ([]string, string) {
	var out []string

	for {
		line, rest, found := strings.Cut(buffer, "\n")
		if !found {
			return out, buffer
		}

		if cmd := strings.TrimSpace(line); cmd != "" {
			out = append(out, cmd)
		}

		buffer = rest
	}
}

// splitMongo takes one JSON document at a time. A document may span lines;
// semicolons, commas and whitespace between documents are ignored. Text that
// does not start a document, such as `db.users.find()`, is handed over a line
// at a time so the parser can report it.
func splitMongo(buffer string) ([]string, string) {
	var out []string

	src := []rune(buffer)
	i := 0

	for {
		for i < len(src) && strings.ContainsRune(" \t\r\n;,", src[i]) {
			i++
		}

		if i >= len(src) {
			return out, ""
		}

		if src[i] != '{' {
			end := i
			for end < len(src) && src[end] != '\n' {
				end++
			}

			if end >= len(src) {
				return out, string(src[i:])
			}

			out = append(out, strings.TrimSpace(string(src[i:end])))
			i = end

			continue
		}

		end, ok := jsonDocumentEnd(src, i)
		if !ok {
			return out, string(src[i:])
		}

		out = append(out, string(src[i:end]))
		i = end
	}
}

// jsonDocumentEnd returns the offset just past the JSON object starting at
// start, or false when it is not complete yet.
func jsonDocumentEnd(src []rune, start int) (int, bool) {
	depth := 0
	inString, escaped := false, false

	for i := start; i < len(src); i++ {
		c := src[i]

		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}

			continue
		}

		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--

			if depth == 0 {
				return i + 1, true
			}
		}
	}

	return 0, false
}
