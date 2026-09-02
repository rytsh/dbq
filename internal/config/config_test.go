package config

import "testing"

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
