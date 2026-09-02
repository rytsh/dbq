package mcpserver

import (
	"strings"
	"testing"

	"github.com/rytsh/dbq/internal/database"
	"github.com/rytsh/dbq/internal/service"
)

func newTestService(t *testing.T) *service.Service {
	t.Helper()

	manager, err := database.NewManager([]database.ConnectionDef{
		{Name: "ro", Type: "sqlite3", Source: ":memory:", Permission: database.PermissionReadOnly, Description: "reports"},
		{Name: "rw", Type: "sqlite3", Source: ":memory:", Permission: database.PermissionFull},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	t.Cleanup(func() { _ = manager.Close() })

	return service.New(manager)
}

func TestInstructions(t *testing.T) {
	svc := newTestService(t)
	text := buildInstructions(svc, Options{Scope: service.NewScope(database.PermissionFull, nil, 25)})

	if n := strings.Count(text, "one statement per call"); n != 1 {
		t.Errorf("'one statement per call' appears %d times, want once:\n%s", n, text)
	}

	// The model has to learn that safe-write means bounded writes before it
	// fails, not from the refusal.
	if !strings.Contains(text, "WHERE") || !strings.Contains(text, "safe-write") {
		t.Errorf("instructions do not explain the bounded-write rule:\n%s", text)
	}

	if !strings.Contains(text, "25 rows") {
		t.Errorf("instructions do not mention the row cap:\n%s", text)
	}

	if !strings.Contains(text, "- ro (type: sqlite3, permission: read-only) — reports") {
		t.Errorf("instructions do not list the connection with its description:\n%s", text)
	}
}

func TestInstructionsReadOnlyEndpoint(t *testing.T) {
	svc := newTestService(t)
	text := buildInstructions(svc, Options{Scope: service.NewScope(database.PermissionReadOnly, nil, 0)})

	if !strings.Contains(text, "This endpoint is read-only") {
		t.Errorf("read-only endpoint is not announced:\n%s", text)
	}
}
