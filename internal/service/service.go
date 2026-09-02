// Package service holds dbq's transport-agnostic business logic used by the CLI,
// health probes and MCP server.
package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rytsh/dbq/internal/database"
)

// Service exposes database operations over a set of named connections.
type Service struct {
	manager *database.Manager
}

// New builds a Service over the given connection manager.
func New(manager *database.Manager) *Service {
	return &Service{manager: manager}
}

// Manager exposes the underlying connection manager for lifecycle handling.
func (s *Service) Manager() *database.Manager {
	return s.manager
}

// Scope is a caller's view of the service: which connections it may see and how
// much it may do with them.
//
// It lets each caller apply its own connection visibility and permission limits
// without duplicating the data access code.
type Scope struct {
	// Cap is the ceiling applied on top of a connection's own permission. The
	// effective permission is the lower of the two. Empty means no extra cap.
	Cap database.Permission
	// Allow restricts which connections are visible. Nil or empty means all.
	Allow map[string]struct{}
	// MaxRows is the default row limit for this scope. Zero uses the database
	// package default.
	MaxRows int
	// MaxCellChars is the default per-value character limit. Zero uses the
	// database package default.
	MaxCellChars int
	// Timeout bounds a single statement. Zero means no dbq-imposed deadline.
	Timeout time.Duration
}

// CanWrite reports whether any modifying statement could succeed in this scope.
// It is false when the ceiling forbids writes, or when every visible connection
// is itself read-only.
func (s *Service) CanWrite(scope Scope) bool {
	if !scope.ceiling().CanWrite() {
		return false
	}

	for _, info := range s.Connections(scope) {
		if info.Permission.CanWrite() {
			return true
		}
	}

	return false
}

// FullScope is the unrestricted scope used by trusted local callers.
var FullScope = Scope{Cap: database.PermissionFull}

// NewScope builds a Scope from a permission ceiling and an allowlist.
func NewScope(ceiling database.Permission, allow []string, maxRows int) Scope {
	scope := Scope{Cap: ceiling, MaxRows: maxRows}

	if len(allow) > 0 {
		scope.Allow = make(map[string]struct{}, len(allow))
		for _, name := range allow {
			scope.Allow[name] = struct{}{}
		}
	}

	return scope
}

func (s Scope) visible(name string) bool {
	if len(s.Allow) == 0 {
		return true
	}

	_, ok := s.Allow[name]

	return ok
}

func (s Scope) ceiling() database.Permission {
	if s.Cap == "" {
		return database.PermissionFull
	}

	return s.Cap
}

// Connections returns the connections visible in the scope, with the effective
// permission already folded in so callers see what they can actually do.
func (s *Service) Connections(scope Scope) []database.ConnectionInfo {
	all := s.manager.List()
	out := make([]database.ConnectionInfo, 0, len(all))

	for _, info := range all {
		if !scope.visible(info.Name) {
			continue
		}

		info.Permission = info.Permission.Min(scope.ceiling())
		out = append(out, info)
	}

	return out
}

// resolve validates that the connection is visible in the scope and returns its
// definition plus the effective permission.
func (s *Service) resolve(scope Scope, name string) (database.ConnectionDef, database.Permission, error) {
	def, err := s.manager.Def(name)
	if err != nil {
		return database.ConnectionDef{}, "", err
	}

	if !scope.visible(def.Name) {
		// Reported as unknown rather than forbidden so a restricted caller
		// cannot enumerate connections it is not allowed to see.
		return database.ConnectionDef{}, "", fmt.Errorf(
			"%w %q", database.ErrUnknownConnection, def.Name,
		)
	}

	return def, def.Permission.Min(scope.ceiling()), nil
}

// Ping verifies that a connection is reachable.
func (s *Service) Ping(ctx context.Context, scope Scope, connection string) error {
	def, _, err := s.resolve(scope, connection)
	if err != nil {
		return err
	}

	return s.manager.Ping(ctx, def.Name)
}

// ListTables returns the tables and views of a connection.
func (s *Service) ListTables(ctx context.Context, scope Scope, connection, schema string) ([]database.Table, error) {
	def, _, err := s.resolve(scope, connection)
	if err != nil {
		return nil, err
	}

	db, err := s.manager.DB(ctx, def.Name)
	if err != nil {
		return nil, err
	}

	return database.ListTables(ctx, db, def.CatalogType(), schema)
}

// DescribeTable returns the column layout of a table.
func (s *Service) DescribeTable(ctx context.Context, scope Scope, connection, schema, table string) (*database.TableDetail, error) {
	def, _, err := s.resolve(scope, connection)
	if err != nil {
		return nil, err
	}

	db, err := s.manager.DB(ctx, def.Name)
	if err != nil {
		return nil, err
	}

	return database.DescribeTable(ctx, db, def.CatalogType(), schema, table)
}

// SchemaContextRequest asks for a compact description of a whole schema.
type SchemaContextRequest struct {
	Connection string
	Schema     string
	// Tables restricts the output to these table names. Empty means all.
	Tables []string
	// MaxTables caps how many tables are described. Zero uses DefaultMaxSchemaTables.
	MaxTables int
}

// DefaultMaxSchemaTables bounds a schema context request. Describing a table is
// one round trip each, so an unbounded request against a large schema would be
// both slow and far too large for a model's context window.
const DefaultMaxSchemaTables = 40

// SchemaContext is a whole-schema summary aimed at an LLM context window.
type SchemaContext struct {
	Connection string                 `json:"connection"`
	Schema     string                 `json:"schema,omitempty"`
	Tables     []database.TableDetail `json:"tables"`
	// TableCount is how many tables exist, before MaxTables was applied.
	TableCount int `json:"table_count"`
	// Truncated is true when TableCount exceeded the cap.
	Truncated bool `json:"truncated,omitempty"`
	// Compact is a one-line-per-table rendering, far cheaper in tokens than the
	// structured form and usually all a model needs to write correct SQL.
	Compact string `json:"compact"`
}

// SchemaContext lists the tables of a connection and describes each one.
func (s *Service) SchemaContext(ctx context.Context, scope Scope, req SchemaContextRequest) (*SchemaContext, error) {
	def, _, err := s.resolve(scope, req.Connection)
	if err != nil {
		return nil, err
	}

	db, err := s.manager.DB(ctx, def.Name)
	if err != nil {
		return nil, err
	}

	tables, err := database.ListTables(ctx, db, def.CatalogType(), req.Schema)
	if err != nil {
		return nil, err
	}

	if len(req.Tables) > 0 {
		wanted := make(map[string]struct{}, len(req.Tables))
		for _, name := range req.Tables {
			wanted[strings.ToLower(name)] = struct{}{}
		}

		filtered := tables[:0]

		for _, t := range tables {
			if _, ok := wanted[strings.ToLower(t.Name)]; ok {
				filtered = append(filtered, t)
			}
		}

		tables = filtered
	}

	out := &SchemaContext{
		Connection: def.Name,
		Schema:     req.Schema,
		TableCount: len(tables),
		Tables:     []database.TableDetail{},
	}

	maxTables := req.MaxTables
	if maxTables <= 0 {
		maxTables = DefaultMaxSchemaTables
	}

	if len(tables) > maxTables {
		tables = tables[:maxTables]
		out.Truncated = true
	}

	for _, t := range tables {
		detail, err := database.DescribeTable(ctx, db, def.CatalogType(), t.Schema, t.Name)
		if err != nil {
			// A table can vanish or be unreadable between listing and
			// describing; skip it rather than failing the whole context.
			continue
		}

		detail.Type = t.Type
		out.Tables = append(out.Tables, *detail)
	}

	out.Compact = compactSchema(out.Tables)

	return out, nil
}

// compactSchema renders each table as a single line, plus its foreign keys:
//
//	users(id INTEGER pk, name TEXT not null, email TEXT)
//	orders(id INTEGER pk, user_id INTEGER not null)
//	  fk: orders.user_id -> users.id
//
// One line per table rather than per column keeps a whole schema affordable in
// a model's context. The foreign keys are what let it write correct JOINs
// instead of guessing at key names, and the VIEW marker stops it generating an
// INSERT against something that cannot accept one.
func compactSchema(tables []database.TableDetail) string {
	var b strings.Builder

	for _, t := range tables {
		name := t.Name
		if t.Schema != "" {
			name = t.Schema + "." + t.Name
		}

		b.WriteString(name)
		b.WriteString("(")

		for i, col := range t.Columns {
			if i > 0 {
				b.WriteString(", ")
			}

			b.WriteString(col.Name)
			b.WriteString(" ")
			b.WriteString(col.Type)

			if col.PrimaryKey {
				b.WriteString(" pk")
			}

			if !col.Nullable && !col.PrimaryKey {
				b.WriteString(" not null")
			}
		}

		b.WriteString(")")

		if strings.EqualFold(t.Type, "VIEW") {
			b.WriteString(" [VIEW, not writable]")
		}

		b.WriteString("\n")

		for _, fk := range t.ForeignKeys {
			target := fk.ReferencedTable
			if fk.ReferencedColumn != "" {
				target += "." + fk.ReferencedColumn
			}

			fmt.Fprintf(&b, "  fk: %s.%s -> %s\n", t.Name, fk.Column, target)
		}
	}

	return b.String()
}

// ExecuteRequest is a single SQL statement to run.
type ExecuteRequest struct {
	// Connection is the required profile name.
	Connection string
	// SQL is the statement. Exactly one statement is expected.
	SQL string
	// MaxRows overrides the scope row limit. Zero uses the scope default.
	MaxRows int
	// MaxCellChars overrides the scope per-value limit. Zero uses the default.
	MaxCellChars int
	// ReadOnly forces the statement to be rejected unless it is a read, no
	// matter what the connection permission allows.
	ReadOnly bool
}

// Execute authorizes and runs a statement.
//
// Authorization happens before the statement reaches the driver: the statement
// is classified, the effective permission (connection permission capped by the
// scope) is checked, and only then is it executed.
func (s *Service) Execute(ctx context.Context, scope Scope, req ExecuteRequest) (*database.Result, error) {
	def, permission, err := s.resolve(scope, req.Connection)
	if err != nil {
		return nil, err
	}

	if req.SQL == "" {
		return nil, fmt.Errorf("sql is required")
	}

	if req.ReadOnly {
		permission = permission.Min(database.PermissionReadOnly)
	}

	if _, err := database.Authorize(def.Name, req.SQL, permission); err != nil {
		return nil, err
	}

	db, err := s.manager.DB(ctx, def.Name)
	if err != nil {
		return nil, err
	}

	return database.Execute(ctx, db, req.SQL, database.QueryOptions{
		MaxRows:      cappedLimit(req.MaxRows, scope.MaxRows, database.DefaultMaxRows),
		MaxCellChars: cappedLimit(req.MaxCellChars, scope.MaxCellChars, database.DefaultMaxCellChars),
		Timeout:      scope.Timeout,
	})
}

// cappedLimit combines a caller's requested limit with the scope's ceiling.
//
// The scope is the operator's decision and always wins: a request may lower
// the limit but never raise it, and a negative request (meaning "unlimited")
// only takes effect when the scope itself is unlimited. Zero on either side
// means "use the default".
func cappedLimit(requested, ceiling, fallback int) int {
	if ceiling == 0 {
		ceiling = fallback
	}

	if ceiling < 0 {
		// The scope imposes no cap; the request decides, and an omitted
		// request inherits the scope's "unlimited".
		if requested == 0 {
			return ceiling
		}

		return requested
	}

	if requested <= 0 || requested > ceiling {
		return ceiling
	}

	return requested
}

// Health pings every connection concurrently, each under its own timeout, and
// reports the outcome per connection. Concurrency matters because one hung
// database must not delay the report on the others, and the timeout is what
// stops it from hanging the probe altogether. Zero timeout means none.
func (s *Service) Health(ctx context.Context, timeout time.Duration) map[string]error {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		statuses = map[string]error{}
	)

	for _, info := range s.Connections(FullScope) {
		wg.Add(1)

		go func(name string) {
			defer wg.Done()

			pingCtx := ctx
			cancel := func() {}
			if timeout > 0 {
				pingCtx, cancel = context.WithTimeout(ctx, timeout)
			}
			defer cancel()

			err := s.Ping(pingCtx, FullScope, name)

			mu.Lock()
			statuses[name] = err
			mu.Unlock()
		}(info.Name)
	}

	wg.Wait()

	return statuses
}
