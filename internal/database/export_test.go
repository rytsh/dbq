package database

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestExportInserts(t *testing.T) {
	db, err := ConnectDB(context.Background(), "sqlite3", ":memory:", PoolConfig{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE source (id INTEGER, name TEXT, note TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO source VALUES (1, 'Ada', 'it''s good'), (2, 'Grace', NULL), (3, 'Linus', 'ok')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	result, err := ExportInserts(t.Context(), db,
		`SELECT id, name, note FROM source ORDER BY id`, &out,
		ExportOptions{Dialect: "sqlite3", Table: "archive.users", BatchSize: 2},
	)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	if result.Rows != 3 || result.Statements != 2 || result.Columns != 3 {
		t.Fatalf("result = %+v", result)
	}

	want := "INSERT INTO \"archive\".\"users\" (\"id\", \"name\", \"note\") VALUES\n" +
		"  (1, 'Ada', 'it''s good'),\n" +
		"  (2, 'Grace', NULL);\n" +
		"INSERT INTO \"archive\".\"users\" (\"id\", \"name\", \"note\") VALUES\n" +
		"  (3, 'Linus', 'ok');\n"
	if out.String() != want {
		t.Errorf("export:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestExportInsertsRowLimitRemovesPartialResultFromCaller(t *testing.T) {
	db, err := ConnectDB(context.Background(), "sqlite3", ":memory:", PoolConfig{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var out bytes.Buffer
	_, err = ExportInserts(t.Context(), db,
		`SELECT 1 AS id UNION ALL SELECT 2`, &out,
		ExportOptions{Dialect: "sqlite3", Table: "target", BatchSize: 1, MaxRows: 1},
	)
	if err == nil || !strings.Contains(err.Error(), "row limit") {
		t.Fatalf("error = %v, want row limit", err)
	}
}

func TestOracleInsertAll(t *testing.T) {
	var out bytes.Buffer
	err := writeInsert(&out, "oracle", "APP.USERS", []string{"id", "name"}, [][]any{{1, "Ada"}, {2, nil}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	want := "INSERT ALL\n" +
		"  INTO \"APP\".\"USERS\" (\"id\", \"name\") VALUES (1, 'Ada')\n" +
		"  INTO \"APP\".\"USERS\" (\"id\", \"name\") VALUES (2, NULL)\n" +
		"SELECT 1 FROM DUAL;\n"
	if out.String() != want {
		t.Errorf("oracle export:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStringLiteralsDoNotExposeExecutableMySQLOrPostgresText(t *testing.T) {
	hostile := `\'; DROP TABLE users; --`
	for _, dialect := range []string{"mysql", "postgresql"} {
		literal := sqlStringLiteral(dialect, hostile)
		if strings.Contains(literal, "DROP TABLE") || !strings.Contains(literal, "5C") {
			t.Errorf("%s literal is not safely hex encoded: %s", dialect, literal)
		}
	}
}

func TestNormalizeExportDialect(t *testing.T) {
	if got, err := NormalizeExportDialect("PGX"); err != nil || got != "postgresql" {
		t.Errorf("PGX = %q, %v", got, err)
	}
	if _, err := NormalizeExportDialect("postgress"); err == nil {
		t.Error("unknown dialect was accepted")
	}
}

func TestExportPreservesUTF8BlobAsBinary(t *testing.T) {
	db, err := ConnectDB(context.Background(), "sqlite3", ":memory:", PoolConfig{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE source (payload BLOB); INSERT INTO source VALUES (X'616263')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	if _, err := ExportInserts(t.Context(), db, `SELECT payload FROM source`, &out,
		ExportOptions{Dialect: "sqlite3", Table: "target"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(out.String(), "X'616263'") {
		t.Errorf("blob was not exported as binary: %s", out.String())
	}
}
