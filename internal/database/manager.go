package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/jmoiron/sqlx"
)

// ErrUnknownConnection is returned when a named connection is not configured.
var ErrUnknownConnection = errors.New("unknown connection")

// ConnectionDef is a named connection profile.
type ConnectionDef struct {
	Name        string
	Type        string
	Dialect     string
	Source      string
	Description string
	Permission  Permission
}

// ConnectionInfo is the redacted view of a connection, safe to return over the
// API or to an MCP client. It never carries the DSN.
type ConnectionInfo struct {
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Dialect     string     `json:"dialect,omitempty"`
	Description string     `json:"description,omitempty"`
	Permission  Permission `json:"permission"`
}

type connEntry struct {
	def ConnectionDef

	mu sync.Mutex
	db *sqlx.DB
}

// CatalogType returns the database dialect used for schema introspection.
// Most drivers identify their dialect directly; ODBC connections may override it.
func (d ConnectionDef) CatalogType() string {
	if d.Dialect != "" {
		return d.Dialect
	}

	return d.Type
}

// Manager owns the connection pools for every configured profile.
//
// Pools are opened lazily on first use. The HTTP server explicitly pings every
// enabled connection at startup; other commands only open the one they use.
type Manager struct {
	entries map[string]*connEntry
	names   []string
}

// NewManager builds a Manager from connection definitions.
// No network I/O happens here.
func NewManager(defs []ConnectionDef) (*Manager, error) {
	if len(defs) == 0 {
		return nil, errors.New("no connections configured")
	}

	m := &Manager{
		entries: make(map[string]*connEntry, len(defs)),
		names:   make([]string, 0, len(defs)),
	}

	for _, def := range defs {
		if def.Name == "" {
			return nil, errors.New("connection name is required")
		}

		if _, dup := m.entries[def.Name]; dup {
			return nil, fmt.Errorf("duplicate connection %q", def.Name)
		}

		if def.Source == "" {
			return nil, fmt.Errorf("connection %q: source is required", def.Name)
		}

		if err := ValidateDriver(def.Type); err != nil {
			return nil, fmt.Errorf("connection %q: %w", def.Name, err)
		}

		if !def.Permission.Valid() {
			return nil, fmt.Errorf("connection %q: invalid permission %q", def.Name, def.Permission)
		}

		m.entries[def.Name] = &connEntry{def: def}
		m.names = append(m.names, def.Name)
	}

	sort.Strings(m.names)

	return m, nil
}

// Names returns the configured connection names, sorted.
func (m *Manager) Names() []string {
	out := make([]string, len(m.names))
	copy(out, m.names)

	return out
}

// List returns the redacted view of every connection.
func (m *Manager) List() []ConnectionInfo {
	out := make([]ConnectionInfo, 0, len(m.names))

	for _, name := range m.names {
		def := m.entries[name].def
		out = append(out, ConnectionInfo{
			Name:        def.Name,
			Type:        def.Type,
			Dialect:     def.Dialect,
			Description: def.Description,
			Permission:  def.Permission,
		})
	}

	return out
}

// Def returns the definition for name.
func (m *Manager) Def(name string) (ConnectionDef, error) {
	entry, err := m.entry(name)
	if err != nil {
		return ConnectionDef{}, err
	}

	return entry.def, nil
}

func (m *Manager) entry(name string) (*connEntry, error) {
	if name == "" {
		return nil, fmt.Errorf("connection is required, available: %v", m.names)
	}

	entry, ok := m.entries[name]
	if !ok {
		return nil, fmt.Errorf("%w %q, available: %v", ErrUnknownConnection, name, m.names)
	}

	return entry, nil
}

// DB returns the pool for name, opening it on first use.
func (m *Manager) DB(ctx context.Context, name string) (*sqlx.DB, error) {
	entry, err := m.entry(name)
	if err != nil {
		return nil, err
	}

	// Held across the connect so concurrent first-use requests do not each open
	// a pool. Different connections have different mutexes, so a slow or dead
	// database never blocks traffic to the others.
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.db != nil {
		return entry.db, nil
	}

	db, err := ConnectDB(ctx, entry.def.Type, entry.def.Source)
	if err != nil {
		return nil, fmt.Errorf("connection %q: %w", entry.def.Name, err)
	}

	slog.Info("connected to database", "connection", entry.def.Name, "type", entry.def.Type)

	entry.db = db

	return db, nil
}

// Ping opens (if needed) and verifies the connection.
func (m *Manager) Ping(ctx context.Context, name string) error {
	db, err := m.DB(ctx, name)
	if err != nil {
		return err
	}

	return db.PingContext(ctx)
}

// Close shuts every open pool down. Errors are joined so one bad pool does not
// hide the others.
func (m *Manager) Close() error {
	var errs []error

	for _, name := range m.names {
		entry := m.entries[name]

		entry.mu.Lock()
		if entry.db != nil {
			if err := entry.db.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing %q: %w", name, err))
			}

			entry.db = nil
		}
		entry.mu.Unlock()
	}

	return errors.Join(errs...)
}
