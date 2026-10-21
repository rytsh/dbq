package database

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{"single", "SELECT 1;", []string{"SELECT 1;"}},
		{"two", "SELECT 1; SELECT 2;", []string{"SELECT 1;", "SELECT 2;"}},
		{"semicolon in string", "SELECT ';' AS a; SELECT 2;", []string{"SELECT ';' AS a;", "SELECT 2;"}},
		{"semicolon in comment", "SELECT 1 -- ;\n; SELECT 2;", []string{"SELECT 1 -- ;\n;", "SELECT 2;"}},
		{"semicolon in block comment", "SELECT /* ; */ 1; SELECT 2;", []string{"SELECT /* ; */ 1;", "SELECT 2;"}},
		{"dollar quoted", "SELECT $$ ; $$; SELECT 2;", []string{"SELECT $$ ; $$;", "SELECT 2;"}},
		{"unterminated tail", "SELECT 1; SELECT 2", []string{"SELECT 1;", "SELECT 2"}},
		{"mysql backslash escape", `SELECT 'it\'s'; SELECT 2;`, []string{`SELECT 'it\'s';`, "SELECT 2;"}},
		{"standard doubled quote", "SELECT 'it''s'; SELECT 2;", []string{"SELECT 'it''s';", "SELECT 2;"}},
		{"blank pieces dropped", " ; ;\n SELECT 1; ; ", []string{"SELECT 1;"}},
		{"only whitespace", "  \n ", nil},
		{"comment only", "-- nothing", nil},
		{"trailing comment dropped", "select 1;\n-- done\n", []string{"select 1;"}},
		{"leading comment kept with statement", "-- header\nselect 1;", []string{"-- header\nselect 1;"}},
		{"block comment only piece", "select 1; /* x */ ;", []string{"select 1;"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SplitStatements(tt.sql); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitStatements(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

func TestSplitComplete(t *testing.T) {
	complete, rest := SplitComplete("SELECT 1; SELECT ';")
	if !reflect.DeepEqual(complete, []string{"SELECT 1;"}) || strings.TrimSpace(rest) != "SELECT ';" {
		t.Errorf("SplitComplete = %q, %q", complete, rest)
	}

	complete, rest = SplitComplete("SELECT 1 -- ;")
	if len(complete) != 0 || rest != "SELECT 1 -- ;" {
		t.Errorf("SplitComplete = %q, %q; a semicolon in a comment must not terminate", complete, rest)
	}

	complete, rest = SplitComplete("SELECT 1;\n")
	if !reflect.DeepEqual(complete, []string{"SELECT 1;"}) || strings.TrimSpace(rest) != "" {
		t.Errorf("SplitComplete = %q, %q", complete, rest)
	}
}

func TestSplitterFor(t *testing.T) {
	tests := []struct {
		language string
		in       string
		want     []string
		rest     string
	}{
		{LanguageRedis, "GET a\n\nSET b \"x;y\"\nINCR", []string{"GET a", `SET b "x;y"`}, "INCR"},
		{LanguageMongoDB, "{\"find\": \"u\",\n \"filter\": {\"a\": \"}\"}};\n{\"count\": \"u\"} {\"ping\"", []string{"{\"find\": \"u\",\n \"filter\": {\"a\": \"}\"}}", `{"count": "u"}`}, `{"ping"`},
		{LanguageMongoDB, "db.users.find()\n{\"ping\": 1}", []string{"db.users.find()", `{"ping": 1}`}, ""},
		{LanguageCQL, "SELECT * FROM t; BEGIN BATCH INSERT INTO t (a) VALUES (1); DELETE FROM t WHERE a = 2; APPLY BATCH; SELECT 1",
			[]string{"SELECT * FROM t;", "BEGIN BATCH INSERT INTO t (a) VALUES (1); DELETE FROM t WHERE a = 2; APPLY BATCH;"}, " SELECT 1"},
		{LanguageCQL, "BEGIN BATCH INSERT INTO t (a) VALUES (1);", nil, "BEGIN BATCH INSERT INTO t (a) VALUES (1);"},
	}

	for _, tt := range tests {
		got, rest := SplitterFor(tt.language)(tt.in)
		if strings.Join(got, "|") != strings.Join(tt.want, "|") || rest != tt.rest {
			t.Errorf("%s split(%q) = %q, %q; want %q, %q", tt.language, tt.in, got, rest, tt.want, tt.rest)
		}
	}
}
