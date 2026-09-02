package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rytsh/dbq/internal/config"
	"github.com/rytsh/dbq/internal/database"
	"github.com/rytsh/dbq/internal/service"
)

func TestCheckConnections(t *testing.T) {
	cfg := &config.Config{
		Connections: map[string]config.Connection{
			"local": {
				Type:       "sqlite3",
				Source:     filepath.Join(t.TempDir(), "test.db"),
				Permission: "read-only",
			},
		},
		Server: config.Server{ConnectionCheckTimeout: time.Second},
	}

	defs, err := cfg.ConnectionDefs()
	if err != nil {
		t.Fatalf("connection definitions: %v", err)
	}
	manager, err := database.NewManager(defs)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	if err := checkConnections(t.Context(), cfg, service.New(manager)); err != nil {
		t.Fatalf("checkConnections: %v", err)
	}
}
