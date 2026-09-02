// Package mcpserver exposes dbq's service layer over the Model Context Protocol.
//
// Tool names are prefixed with "dbq_" so they stay distinguishable when an agent
// has several database MCP servers connected at once.
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/dbq/internal/database"
	"github.com/rytsh/dbq/internal/service"
)

// ToolPrefix namespaces every tool dbq registers.
const ToolPrefix = "dbq_"

// Options configures the MCP server.
type Options struct {
	// Name and Version identify the server to clients.
	Name    string
	Version string
	// Scope limits which connections are exposed and what may be run on them.
	Scope service.Scope
	// MaxSchemaTables caps dbq_schema_context. Zero uses the service default.
	MaxSchemaTables int
	// Stateless disables MCP session state, required behind a load balancer.
	Stateless bool
	// JSONResponse returns application/json instead of text/event-stream.
	JSONResponse bool
	// SessionTimeout closes idle sessions. Zero means never.
	SessionTimeout time.Duration
	// Logger receives MCP protocol logs. Nil disables them.
	Logger *slog.Logger
}

type minLevelHandler struct {
	slog.Handler
	level slog.Level
}

func (h minLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level && h.Handler.Enabled(ctx, level)
}

// New builds an MCP server exposing dbq's tools.
func New(svc *service.Service, opts Options) *mcp.Server {
	logger := opts.Logger
	if logger != nil {
		// The SDK emits a connect/disconnect INFO trio for every stateless HTTP
		// request, where an empty session ID is expected. Keep actionable logs.
		logger = slog.New(minLevelHandler{Handler: logger.Handler(), level: slog.LevelWarn})
	}

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:        opts.Name,
			Version:     opts.Version,
			Title:       "dbq",
			Description: "Inspect and query SQL databases through dbq's configured connections.",
		},
		&mcp.ServerOptions{
			Instructions: buildInstructions(svc, opts),
			Logger:       logger,
		},
	)

	registerTools(server, svc, opts)

	return server
}

// Handler returns the streamable HTTP handler to mount on the web server.
//
// The same *mcp.Server instance is reused for every session: it holds no
// per-client state, and the SDK explicitly supports getServer returning the
// same server multiple times.
func Handler(server *mcp.Server, opts Options) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:      opts.Stateless,
			JSONResponse:   opts.JSONResponse,
			SessionTimeout: opts.SessionTimeout,
			Logger:         opts.Logger,
		},
	)
}

// buildInstructions is the system-level prompt the client shows the model. It
// spells out the intended workflow, the permission model and the row caps,
// because those are the three things a model otherwise gets wrong: it guesses
// table names, it tries writes on read-only connections, and it assumes a
// truncated result set is the whole table.
func buildInstructions(svc *service.Service, opts Options) string {
	var b strings.Builder

	maxRows := opts.Scope.MaxRows
	if maxRows == 0 {
		maxRows = database.DefaultMaxRows
	}

	b.WriteString("dbq gives you read and (where permitted) write access to SQL databases ")
	b.WriteString("that an operator has configured as named connections.\n\n")

	b.WriteString("Workflow:\n")
	b.WriteString("1. Call " + ToolPrefix + "list_connections to see which databases you can reach.\n")
	b.WriteString("2. Call " + ToolPrefix + "schema_context for a compact overview of a database, ")
	b.WriteString("or " + ToolPrefix + "list_tables plus " + ToolPrefix + "describe_table when you only need a few tables.\n")
	b.WriteString("3. Write SQL against the real column names you just read, then run it with " + ToolPrefix + "query.\n\n")

	b.WriteString("Rules:\n")
	b.WriteString("- Never invent table or column names. Inspect the schema first.\n")
	b.WriteString("- " + ToolPrefix + "query only runs read statements. Use " + ToolPrefix + "execute for INSERT/UPDATE/DELETE/DDL.\n")
	if maxRows > 0 {
		fmt.Fprintf(&b, "- Results are capped at %d rows. ", maxRows)
	} else {
		b.WriteString("- Results are not capped by the server. ")
	}

	b.WriteString("Use LIMIT and aggregates instead of pulling whole tables; ")
	b.WriteString("if truncated is true you are seeing a partial result, not the full answer.\n")
	b.WriteString("- Long text values are shortened. If cells_truncated is non-zero, the values shown are prefixes; ")
	b.WriteString("narrow the query to the row and column you need rather than raising the limit blindly.\n")
	b.WriteString("- Send exactly one statement per call. Statements separated by semicolons are refused.\n")
	b.WriteString("- Write SQL in the dialect of the connection's type (pgx = PostgreSQL, sqlite3 = SQLite, ")
	b.WriteString("sqlserver = SQL Server, godror = Oracle, odbc = the ODBC target).\n")
	b.WriteString("- Each connection has a permission level. read-only rejects every write. safe-write allows ")
	b.WriteString("only writes with a bounded reach: INSERT ... VALUES, and UPDATE or DELETE on a single table ")
	b.WriteString("with a WHERE clause that selects specific rows. Writes without an effective WHERE clause, ")
	b.WriteString("INSERT ... SELECT, upserts, MERGE, joined writes and subqueries in WHERE need full, as does DDL. ")
	b.WriteString("A rejected statement is a policy decision, not a bug: do not try to work around it.\n")
	b.WriteString("- Always scope UPDATE and DELETE with a specific WHERE clause. Never rely on WHERE 1=1.\n\n")

	if !svc.CanWrite(opts.Scope) {
		b.WriteString("- This endpoint is read-only: no write tool is available. ")
		b.WriteString("If the user needs a write, tell them to use a write-enabled endpoint; do not look for a workaround.\n")
	}

	b.WriteString("\nAvailable connections:\n")

	for _, info := range svc.Connections(opts.Scope) {
		fmt.Fprintf(&b, "- %s (type: %s, permission: %s)", info.Name, info.Type, info.Permission)

		if info.Description != "" {
			fmt.Fprintf(&b, " — %s", info.Description)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// Tool argument and result types. The jsonschema struct tag becomes the
// property description in the tool's input schema, which is what the model
// actually reads when it fills the arguments in.
// ---------------------------------------------------------------------------

type listConnectionsInput struct{}

type listConnectionsOutput struct {
	Connections []database.ConnectionInfo `json:"connections"`
}

type listTablesInput struct {
	Connection string `json:"connection" jsonschema:"connection name from dbq_list_connections"`
	Schema     string `json:"schema,omitempty" jsonschema:"restrict the listing to a single schema; omit to list every non-system schema"`
}

type listTablesOutput struct {
	Connection string           `json:"connection"`
	Tables     []database.Table `json:"tables"`
	Count      int              `json:"count"`
}

type describeTableInput struct {
	Connection string `json:"connection" jsonschema:"connection name from dbq_list_connections"`
	Table      string `json:"table" jsonschema:"table or view name; schema.table is also accepted"`
	Schema     string `json:"schema,omitempty" jsonschema:"schema the table lives in; omit if the table argument is already qualified"`
}

type schemaContextInput struct {
	Connection string   `json:"connection" jsonschema:"connection name from dbq_list_connections"`
	Schema     string   `json:"schema,omitempty" jsonschema:"restrict the context to a single schema; omit for every non-system schema"`
	Tables     []string `json:"tables,omitempty" jsonschema:"describe only these tables; omit to describe everything up to the server cap"`
}

type queryInput struct {
	Connection string `json:"connection" jsonschema:"connection name from dbq_list_connections"`
	SQL        string `json:"sql" jsonschema:"exactly one read-only SQL statement (SELECT, SHOW, EXPLAIN, or WITH ... SELECT) in the connection's dialect; do not send several statements separated by semicolons"`
	MaxRows    int    `json:"max_rows,omitempty" jsonschema:"rows to return; may be lower than the server's cap, never higher"`
	MaxChars   int    `json:"max_chars,omitempty" jsonschema:"characters to keep per text value before it is shortened; may be lower than the server's cap, never higher, so narrow the query to the row and column you need instead of asking for more"`
}

type executeInput struct {
	Connection string `json:"connection" jsonschema:"connection name from dbq_list_connections"`
	SQL        string `json:"sql" jsonschema:"exactly one data- or schema-modifying SQL statement; UPDATE and DELETE must carry a WHERE clause that selects specific rows"`
}

type queryOutput struct {
	Connection string        `json:"connection"`
	Kind       database.Kind `json:"kind"`
	Columns    []string      `json:"columns,omitempty"`
	Rows       [][]any       `json:"rows,omitempty"`
	RowCount   int           `json:"row_count"`
	// Truncated means the row cap was hit and further rows exist.
	Truncated bool `json:"truncated,omitempty"`
	// CellsTruncated counts values shortened to the character cap. Non-zero
	// means the values shown are prefixes, not the stored data.
	CellsTruncated int    `json:"cells_truncated,omitempty"`
	RowsAffected   *int64 `json:"rows_affected,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
}

func registerTools(server *mcp.Server, svc *service.Service, opts Options) {
	scope := opts.Scope

	// Pointer-valued hints have to be addressable; these are the shared values.
	closedWorld := false
	destructive := true

	readOnlyAnnotations := func(title string) *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{
			Title:          title,
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  &closedWorld,
		}
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolPrefix + "list_connections",
		Title:       "List database connections",
		Annotations: readOnlyAnnotations("List database connections"),
		Description: "List the databases dbq can reach, with each connection's driver type and " +
			"permission level (read-only, safe-write or full). Call this first: every other tool " +
			"takes one of these connection names.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ listConnectionsInput) (*mcp.CallToolResult, listConnectionsOutput, error) {
		return nil, listConnectionsOutput{Connections: svc.Connections(scope)}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolPrefix + "list_tables",
		Title:       "List tables and views",
		Annotations: readOnlyAnnotations("List tables and views"),
		Description: "List the tables and views of a connection, with their schema. " +
			"Use this to find the right table name before describing or querying it. " +
			"For a full schema overview in one call, prefer " + ToolPrefix + "schema_context.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listTablesInput) (*mcp.CallToolResult, listTablesOutput, error) {
		tables, err := svc.ListTables(ctx, scope, in.Connection, in.Schema)
		if err != nil {
			return nil, listTablesOutput{}, err
		}

		return nil, listTablesOutput{
			Connection: in.Connection,
			Tables:     tables,
			Count:      len(tables),
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolPrefix + "describe_table",
		Title:       "Describe a table",
		Annotations: readOnlyAnnotations("Describe a table"),
		Description: "Return one table's columns: name, data type, nullability, default value and " +
			"whether it is part of the primary key. Use it to get exact column names and types " +
			"before writing SQL, so you never have to guess.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in describeTableInput) (*mcp.CallToolResult, *database.TableDetail, error) {
		detail, err := svc.DescribeTable(ctx, scope, in.Connection, in.Schema, in.Table)
		if err != nil {
			return nil, nil, err
		}

		return nil, detail, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolPrefix + "schema_context",
		Title:       "Get compact schema context",
		Annotations: readOnlyAnnotations("Get compact schema context"),
		Description: "Return a compact, token-efficient description of a whole database: every table " +
			"with its columns, types and primary keys, rendered one line per table. This is the " +
			"cheapest way to learn a schema before writing SQL. Large schemas are truncated, so pass " +
			"the tables argument when you already know which tables you care about.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in schemaContextInput) (*mcp.CallToolResult, *service.SchemaContext, error) {
		out, err := svc.SchemaContext(ctx, scope, service.SchemaContextRequest{
			Connection: in.Connection,
			Schema:     in.Schema,
			Tables:     in.Tables,
			MaxTables:  opts.MaxSchemaTables,
		})
		if err != nil {
			return nil, nil, err
		}

		// The compact rendering is what the model should read; hand it over as
		// the text content instead of letting the SDK dump the full JSON.
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: schemaText(out)}},
		}, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:  ToolPrefix + "query",
		Title: "Run a read-only query",
		Annotations: &mcp.ToolAnnotations{
			Title:         "Run a read-only query",
			ReadOnlyHint:  true,
			OpenWorldHint: &closedWorld,
		},
		Description: "Run one read-only SQL statement (SELECT, SHOW, EXPLAIN, WITH ... SELECT) and " +
			"return the rows. Writes and DDL are always refused here regardless of the connection's " +
			"permission, so this tool is safe to call freely. Results are capped: if truncated is " +
			"true, narrow the query with a WHERE clause or aggregate instead of paging blindly.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, queryOutput, error) {
		res, err := svc.Execute(ctx, scope, service.ExecuteRequest{
			Connection:   in.Connection,
			SQL:          in.SQL,
			MaxRows:      in.MaxRows,
			MaxCellChars: in.MaxChars,
			ReadOnly:     true,
		})
		if err != nil {
			return nil, queryOutput{}, err
		}

		return nil, toOutput(in.Connection, res), nil
	})

	// A tool that can never succeed is worse than an absent one: the model
	// sees it, tries it, is refused, and burns turns rediscovering the policy.
	// When the endpoint's ceiling forbids writes, or every visible connection
	// is itself read-only, the write tool is simply not registered.
	// Enforcement still happens in the service layer either way.
	if !svc.CanWrite(scope) {
		return
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:  ToolPrefix + "execute",
		Title: "Execute a modifying statement",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Execute a modifying statement",
			DestructiveHint: &destructive,
			OpenWorldHint:   &closedWorld,
		},
		Description: "Run one data-modifying or schema-modifying SQL statement and return the number " +
			"of affected rows. The statement is refused unless the connection's permission level " +
			"allows it: read-only refuses everything; safe-write allows INSERT ... VALUES and " +
			"single-table UPDATE/DELETE with a WHERE clause that selects specific rows; full is " +
			"needed for DDL, writes without an effective WHERE, INSERT ... SELECT, upserts, MERGE " +
			"and joined writes. Always scope UPDATE and DELETE with a specific WHERE clause, and " +
			"confirm the intent with the user before running anything destructive.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in executeInput) (*mcp.CallToolResult, queryOutput, error) {
		res, err := svc.Execute(ctx, scope, service.ExecuteRequest{
			Connection: in.Connection,
			SQL:        in.SQL,
		})
		if err != nil {
			return nil, queryOutput{}, err
		}

		return nil, toOutput(in.Connection, res), nil
	})
}

func schemaText(ctx *service.SchemaContext) string {
	var b strings.Builder

	fmt.Fprintf(&b, "connection: %s\n", ctx.Connection)

	if ctx.Schema != "" {
		fmt.Fprintf(&b, "schema: %s\n", ctx.Schema)
	}

	fmt.Fprintf(&b, "tables described: %d of %d\n", len(ctx.Tables), ctx.TableCount)

	if ctx.Truncated {
		b.WriteString("truncated: pass the tables argument to describe the ones you need\n")
	}

	b.WriteString("\n")
	b.WriteString(ctx.Compact)

	return b.String()
}

func toOutput(connection string, res *database.Result) queryOutput {
	return queryOutput{
		Connection:     connection,
		Kind:           res.Kind,
		Columns:        res.Columns,
		Rows:           res.Rows,
		RowCount:       res.RowCount,
		Truncated:      res.Truncated,
		CellsTruncated: res.CellsTruncated,
		RowsAffected:   res.RowsAffected,
		DurationMS:     res.DurationMS,
	}
}
