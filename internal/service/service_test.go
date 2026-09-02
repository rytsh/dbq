package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rytsh/dbq/internal/database"
)

func newTestService(t *testing.T) *Service {
	t.Helper()

	manager, err := database.NewManager([]database.ConnectionDef{
		{Name: "ro", Type: "sqlite3", Source: ":memory:", Permission: database.PermissionReadOnly},
		{Name: "rw", Type: "sqlite3", Source: ":memory:", Permission: database.PermissionFull},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	t.Cleanup(func() { _ = manager.Close() })

	return New(manager)
}

const hundredRows = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 100) SELECT x FROM c"

// TestExecuteRowCapCannotBeExceeded is the promise the MCP tool description
// makes: a caller may lower the row limit but never raise it past the scope,
// and cannot disable it with a negative value.
func TestExecuteRowCapCannotBeExceeded(t *testing.T) {
	svc := newTestService(t)
	scope := NewScope(database.PermissionReadOnly, nil, 5)

	tests := []struct {
		name     string
		maxRows  int
		wantRows int
	}{
		{"default uses scope", 0, 5},
		{"lower than scope", 3, 3},
		{"higher than scope is capped", 50, 5},
		{"negative is capped", -1, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := svc.Execute(context.Background(), scope, ExecuteRequest{
				Connection: "ro", SQL: hundredRows, MaxRows: tt.maxRows, ReadOnly: true,
			})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}

			if res.RowCount != tt.wantRows || !res.Truncated {
				t.Errorf("rows = %d truncated = %v, want %d rows truncated", res.RowCount, res.Truncated, tt.wantRows)
			}
		})
	}
}

// TestExecuteUnlimitedScope keeps the REPL behaviour: a scope with a negative
// limit imposes no cap at all.
func TestExecuteUnlimitedScope(t *testing.T) {
	svc := newTestService(t)
	scope := FullScope
	scope.MaxRows = -1

	for _, maxRows := range []int{0, -1} {
		res, err := svc.Execute(context.Background(), scope, ExecuteRequest{
			Connection: "ro", SQL: hundredRows, MaxRows: maxRows,
		})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}

		if res.RowCount != 100 || res.Truncated {
			t.Errorf("max_rows=%d: rows = %d truncated = %v, want 100 rows", maxRows, res.RowCount, res.Truncated)
		}
	}

	res, err := svc.Execute(context.Background(), scope, ExecuteRequest{
		Connection: "ro", SQL: hundredRows, MaxRows: 7,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if res.RowCount != 7 {
		t.Errorf("rows = %d, want a request below an unlimited scope to be honoured", res.RowCount)
	}
}

func TestExecuteCellCapCannotBeExceeded(t *testing.T) {
	svc := newTestService(t)
	scope := NewScope(database.PermissionReadOnly, nil, 5)
	scope.MaxCellChars = 4

	for _, maxChars := range []int{0, 100, -1} {
		res, err := svc.Execute(context.Background(), scope, ExecuteRequest{
			Connection: "ro", SQL: "SELECT 'abcdefghij' AS v", MaxCellChars: maxChars, ReadOnly: true,
		})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}

		cell, _ := res.Rows[0][0].(string)
		if res.CellsTruncated != 1 || !strings.HasPrefix(cell, "abcd…") {
			t.Errorf("max_chars=%d: cell = %q truncated = %d, want a 4-char prefix", maxChars, cell, res.CellsTruncated)
		}
	}
}

func TestScopeHidesConnections(t *testing.T) {
	svc := newTestService(t)
	scope := NewScope(database.PermissionFull, []string{"ro"}, 0)

	if got := svc.Connections(scope); len(got) != 1 || got[0].Name != "ro" {
		t.Errorf("connections = %v, want only ro", got)
	}

	// A hidden connection reads as unknown, not forbidden, so it cannot be enumerated.
	_, err := svc.Execute(context.Background(), scope, ExecuteRequest{Connection: "rw", SQL: "SELECT 1"})
	if err == nil || !strings.Contains(err.Error(), "unknown connection") {
		t.Errorf("err = %v, want unknown connection", err)
	}

	if svc.CanWrite(scope) {
		t.Error("CanWrite = true for a scope whose only connection is read-only")
	}
}

func TestHealth(t *testing.T) {
	svc := newTestService(t)

	statuses := svc.Health(context.Background(), time.Second)
	if len(statuses) != 2 {
		t.Fatalf("statuses = %v, want 2", statuses)
	}

	for name, err := range statuses {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
