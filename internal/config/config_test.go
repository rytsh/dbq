package config

import (
	"testing"
	"time"

	"github.com/rytsh/dbq/internal/database"
)

func TestConnectionDefsSkipsDisabled(t *testing.T) {
	cfg := &Config{Connections: map[string]Connection{
		"enabled": {
			Type:       "odbc",
			Dialect:    "ingres",
			Source:     "DSN=bas",
			Permission: "read-only",
		},
		"disabled": {
			Disabled:   true,
			Type:       "invalid-driver",
			Permission: "invalid-permission",
		},
	}}

	defs, err := cfg.ConnectionDefs()
	if err != nil {
		t.Fatalf("ConnectionDefs: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("definitions = %d, want 1", len(defs))
	}
	if defs[0].Name != "enabled" || defs[0].CatalogType() != "ingres" {
		t.Errorf("definition = %+v, want enabled Ingres connection", defs[0])
	}
}

// TestConnectionDefsMergePool checks that a connection's pool settings override
// the global ones field by field, so a profile only has to name what differs.
func TestConnectionDefsMergePool(t *testing.T) {
	cfg := &Config{
		Pool: Pool{MaxOpen: 8, MaxIdle: 4, MaxLifetime: 10 * time.Minute, MaxIdleTime: time.Minute},
		Connections: map[string]Connection{
			"inherits": {Type: "sqlite3", Source: ":memory:"},
			"overrides": {
				Type: "sqlite3", Source: ":memory:",
				Pool: Pool{MaxOpen: 1, MaxLifetime: time.Hour},
			},
		},
	}

	defs, err := cfg.ConnectionDefs()
	if err != nil {
		t.Fatalf("ConnectionDefs: %v", err)
	}

	byName := map[string]database.PoolConfig{}
	for _, def := range defs {
		byName[def.Name] = def.Pool
	}

	want := map[string]database.PoolConfig{
		"inherits":  {MaxOpen: 8, MaxIdle: 4, MaxLifetime: 10 * time.Minute, MaxIdleTime: time.Minute},
		"overrides": {MaxOpen: 1, MaxIdle: 4, MaxLifetime: time.Hour, MaxIdleTime: time.Minute},
	}

	for name, pool := range want {
		if byName[name] != pool {
			t.Errorf("%s pool = %+v, want %+v", name, byName[name], pool)
		}
	}
}
