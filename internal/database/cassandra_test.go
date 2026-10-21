package database

import "testing"

func TestAnalyzeCQL(t *testing.T) {
	tests := []struct {
		name      string
		cql       string
		kind      Kind
		unbounded bool
		refused   bool
	}{
		{"select", "SELECT * FROM ks.users WHERE id = 1", KindRead, false, false},
		{"select trailing semicolon", "SELECT * FROM users;", KindRead, false, false},
		{"describe", "DESCRIBE TABLE ks.users", KindRead, false, false},
		{"list roles", "LIST ROLES", KindRead, false, false},
		{"insert", "INSERT INTO users (id, name) VALUES (1, 'a')", KindWrite, false, false},
		{"insert json", `INSERT INTO users JSON '{"id": 1}'`, KindWrite, false, false},
		{"update keyed", "UPDATE users SET name = 'b' WHERE id = 1", KindWrite, false, false},
		{"update lwt", "UPDATE users SET name = 'b' WHERE id = 1 IF name = 'a'", KindWrite, false, false},
		{"delete keyed", "DELETE FROM users WHERE id = 1", KindWrite, false, false},
		{"delete no where", "DELETE FROM users", KindWrite, true, false},
		{"delete tautology", "DELETE FROM users WHERE 1 = 1", KindWrite, true, false},
		{"update self compare", "UPDATE users SET a = 1 WHERE id = id", KindWrite, true, false},
		{"truncate", "TRUNCATE users", KindSchema, false, false},
		{"create table", "CREATE TABLE t (id int PRIMARY KEY)", KindSchema, false, false},
		{"drop keyspace", "DROP KEYSPACE ks", KindSchema, false, false},
		{"grant", "GRANT SELECT ON ks.t TO bob", KindSchema, false, false},
		{"use", "USE ks", KindSession, false, true},
		{"two statements", "SELECT * FROM t; DROP TABLE t", KindUnknown, false, true},
		{"semicolon in string", "SELECT * FROM t WHERE a = ';DROP TABLE t'", KindRead, false, false},
		{"double slash comment", "SELECT * FROM t // ; DROP TABLE t", KindRead, false, false},
		// # is not a comment in CQL, so the ; after it is real.
		{"hash is not a comment", "SELECT * FROM t # ; DROP TABLE t", KindUnknown, false, true},
		{"dollar string", "SELECT * FROM t WHERE a = $$;DROP TABLE t$$", KindRead, false, false},
		{"list literal", "UPDATE t SET l = l + [1, 2] WHERE id = 1", KindWrite, false, false},

		{"batch", "BEGIN BATCH INSERT INTO t (id) VALUES (1); UPDATE t SET a = 1 WHERE id = 2; APPLY BATCH", KindWrite, false, false},
		{"unlogged batch with timestamp", "BEGIN UNLOGGED BATCH USING TIMESTAMP 5 INSERT INTO t (id) VALUES (1); APPLY BATCH;", KindWrite, false, false},
		{"batch with unbounded member", "BEGIN BATCH INSERT INTO t (id) VALUES (1); DELETE FROM t; APPLY BATCH", KindWrite, true, false},
		{"batch hiding ddl", "BEGIN BATCH INSERT INTO t (id) VALUES (1); DROP TABLE t; APPLY BATCH", KindUnknown, false, true},
		{"batch without apply", "BEGIN BATCH INSERT INTO t (id) VALUES (1);", KindUnknown, false, true},
		{"statement after batch", "BEGIN BATCH INSERT INTO t (id) VALUES (1); APPLY BATCH; DROP TABLE t", KindUnknown, false, true},
		{"batch header smuggling", "BEGIN BATCH DROP TABLE t INSERT INTO t (id) VALUES (1); APPLY BATCH", KindUnknown, false, true},
		{"apply inside member", "BEGIN BATCH INSERT INTO t (id) VALUES (1) APPLY BATCH; DROP TABLE x; APPLY BATCH", KindUnknown, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AnalyzeCQL(tt.cql)
			if got.Kind != tt.kind || got.Unbounded != tt.unbounded || got.Refused != tt.refused {
				t.Errorf("AnalyzeCQL(%q) = %+v, want kind=%s unbounded=%v refused=%v",
					tt.cql, got, tt.kind, tt.unbounded, tt.refused)
			}
		})
	}
}

func TestParseCassandraSource(t *testing.T) {
	cluster, err := parseCassandraSource("cassandra://bob:s%40cret@h1:9042,h2/ks?consistency=one&timeout=3s")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(cluster.Hosts) != 2 || cluster.Hosts[0] != "h1:9042" || cluster.Hosts[1] != "h2" {
		t.Errorf("hosts = %v", cluster.Hosts)
	}

	if cluster.Keyspace != "ks" || cluster.Consistency.String() != "ONE" || cluster.Timeout.String() != "3s" {
		t.Errorf("cluster = keyspace %q consistency %s timeout %s", cluster.Keyspace, cluster.Consistency, cluster.Timeout)
	}

	if cluster.Authenticator == nil {
		t.Error("credentials were dropped")
	}

	for _, bad := range []string{"h1:9042/ks", "cassandra:///ks", "cassandra://h1/ks?bogus=1"} {
		if _, err := parseCassandraSource(bad); err == nil {
			t.Errorf("parseCassandraSource(%q) = nil error", bad)
		}
	}
}
