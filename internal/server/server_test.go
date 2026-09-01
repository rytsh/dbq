package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rytsh/dbq/internal/config"
	"github.com/rytsh/dbq/internal/database"
	"github.com/rytsh/dbq/internal/server"
	"github.com/rytsh/dbq/internal/service"
)

// newTestServer spins up dbq over two SQLite connections: "ro" is read-only and
// "rw" has full access, so both sides of the permission gate are exercised.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "test.db")

	cfg := &config.Config{
		DefaultConnection: "ro",
		Connections: map[string]config.Connection{
			"ro": {Type: "sqlite3", Source: dsn, Permission: "read-only", Description: "read only"},
			"rw": {Type: "sqlite3", Source: dsn, Permission: "full"},
		},
		Server: config.Server{Port: "0", ShutdownTimeout: time.Second},
		MCP: config.MCP{
			Enabled:   true,
			Path:      "/mcp",
			MaxRows:   10,
			Stateless: true,
			Endpoints: config.Endpoints{
				ReadOnly:  config.Endpoint{Enabled: true},
				SafeWrite: config.Endpoint{Enabled: true},
			},
		},
	}

	defs, err := cfg.ConnectionDefs()
	if err != nil {
		t.Fatalf("connection defs: %v", err)
	}

	manager, err := database.NewManager(defs, cfg.DefaultConnection)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	t.Cleanup(func() { _ = manager.Close() })

	svc := service.New(manager)

	// Seed through the full-access connection.
	seed := []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id))`,
		`CREATE VIEW active_users AS SELECT id, name FROM users`,
		`INSERT INTO users (id, name, email) VALUES (1, 'ada', 'ada@example.com')`,
		`INSERT INTO users (id, name, email) VALUES (2, 'grace', NULL)`,
	}

	for _, sql := range seed {
		if _, err := svc.Execute(t.Context(), service.FullScope, service.ExecuteRequest{
			Connection: "rw", SQL: sql,
		}); err != nil {
			t.Fatalf("seeding %q: %v", sql, err)
		}
	}

	srv, err := server.New(cfg, svc, "test")
	if err != nil {
		t.Fatalf("server: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Status      string            `json:"status"`
		Connections map[string]string `json:"connections"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}

	if len(body.Connections) != 2 { //nolint:mnd // ro + rw
		t.Errorf("connections = %v, want 2 entries", body.Connections)
	}
}

func TestRESTQuery(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Post(
		ts.URL+"/api/v1/connections/ro/query", "application/json",
		strings.NewReader(`{"sql":"SELECT id, name FROM users ORDER BY id"}`),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var res database.Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if res.RowCount != 2 {
		t.Fatalf("row_count = %d, want 2", res.RowCount)
	}

	if got := res.Rows[0][1]; got != "ada" {
		t.Errorf("rows[0][1] = %v, want ada", got)
	}
}

// TestRESTQueryPermissionDenied checks that the read-only connection rejects a
// write even though the REST API itself applies no ceiling.
func TestRESTQueryPermissionDenied(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Post(
		ts.URL+"/api/v1/connections/ro/query", "application/json",
		strings.NewReader(`{"sql":"DELETE FROM users"}`),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestRESTUnknownConnection(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/api/v1/connections/nope/tables")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRESTDescribeTable(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/api/v1/connections/ro/tables/users")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var detail database.TableDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(detail.Columns) != 3 {
		t.Fatalf("columns = %d, want 3", len(detail.Columns))
	}

	if !detail.Columns[0].PrimaryKey {
		t.Error("id should be reported as primary key")
	}

	if detail.Columns[1].Nullable {
		t.Error("name is NOT NULL, should not be nullable")
	}

	if !detail.Columns[2].Nullable {
		t.Error("email should be nullable")
	}
}

// connectMCP performs a real MCP handshake against one of the permission-scoped
// endpoints, e.g. path "/mcp/read-only".
func connectMCP(t *testing.T, ts *httptest.Server, path string) *mcp.ClientSession {
	t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "dbq-test", Version: "v0"}, nil)

	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + path,
		HTTPClient: ts.Client(),
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect %s: %v", path, err)
	}

	t.Cleanup(func() { _ = session.Close() })

	return session
}

const (
	pathReadOnly  = "/mcp/read-only"
	pathSafeWrite = "/mcp/safe-write"
	pathFull      = "/mcp/full"
)

func TestMCPListTools(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}

	want := []string{
		"dbq_list_connections", "dbq_list_tables", "dbq_describe_table",
		"dbq_schema_context", "dbq_query",
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("missing tool %q, have %v", name, got)
		}
	}

	// A write tool that can never succeed would only invite the model to try
	// it and burn turns on a refusal.
	if got["dbq_execute"] {
		t.Error("read-only endpoint must not advertise dbq_execute")
	}
}

// TestMCPWriteToolPresentWhenWritable is the counterpart: the safe-write
// endpoint does expose the write tool.
func TestMCPWriteToolPresentWhenWritable(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathSafeWrite)

	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	for _, tool := range res.Tools {
		if tool.Name == "dbq_execute" {
			return
		}
	}

	t.Error("safe-write endpoint must advertise dbq_execute")
}

func TestMCPQuery(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_query",
		Arguments: map[string]any{"connection": "ro", "sql": "SELECT name FROM users ORDER BY id"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if res.IsError {
		t.Fatalf("tool returned error: %v", contentText(res))
	}

	var out struct {
		RowCount int     `json:"row_count"`
		Rows     [][]any `json:"rows"`
	}

	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}

	if out.RowCount != 2 {
		t.Fatalf("row_count = %d, want 2", out.RowCount)
	}
}

// TestMCPReadOnlyEndpointRejectsWrite is the core of the path split: the "rw"
// connection grants full access, but a client that arrived on the read-only
// endpoint must still be refused.
func TestMCPReadOnlyEndpointRejectsWrite(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	// The write tool is not advertised here, and calling it anyway must still
	// fail rather than fall through to some default.
	_, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_execute",
		Arguments: map[string]any{"connection": "rw", "sql": "DELETE FROM users"},
	})
	if err == nil {
		t.Fatal("the read-only endpoint must not execute a DELETE, even on a full-access connection")
	}

	assertRowCount(t, ts, 2)
}

// TestMCPReadOnlyQueryRejectsWrite covers the same policy through the tool that
// is advertised on the read-only endpoint.
func TestMCPReadOnlyQueryRejectsWrite(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_query",
		Arguments: map[string]any{"connection": "rw", "sql": "DELETE FROM users"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if !res.IsError {
		t.Fatal("dbq_query must reject a DELETE")
	}

	assertRowCount(t, ts, 2)
}

// TestMCPUnboundedWriteNeedsFullAccess checks the blast-radius axis end to end:
// the safe-write endpoint runs a scoped DELETE but refuses an unscoped one.
func TestMCPUnboundedWriteNeedsFullAccess(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathSafeWrite)

	for _, sql := range []string{"DELETE FROM users", "DELETE FROM users WHERE 1=1"} {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      "dbq_execute",
			Arguments: map[string]any{"connection": "rw", "sql": sql},
		})
		if err != nil {
			t.Fatalf("call tool: %v", err)
		}

		if !res.IsError {
			t.Fatalf("safe-write must refuse the unbounded statement %q", sql)
		}
	}

	assertRowCount(t, ts, 2)
}

// TestMCPBatchIsRefused covers the multi-statement bypass.
func TestMCPBatchIsRefused(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_query",
		Arguments: map[string]any{"connection": "rw", "sql": "SELECT 1; DROP TABLE users"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if !res.IsError {
		t.Fatal("a two-statement batch must be refused")
	}

	assertRowCount(t, ts, 2)
}

// TestMCPSafeWriteEndpointAllowsDML checks the other side: the same statement on
// the safe-write endpoint goes through, because the connection permits it.
func TestMCPSafeWriteEndpointAllowsDML(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathSafeWrite)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_execute",
		Arguments: map[string]any{"connection": "rw", "sql": "DELETE FROM users WHERE id = 2"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if res.IsError {
		t.Fatalf("safe-write endpoint refused a DELETE: %v", contentText(res))
	}

	assertRowCount(t, ts, 1)
}

// TestMCPSafeWriteEndpointRejectsDDL checks that safe-write stops short of DDL.
func TestMCPSafeWriteEndpointRejectsDDL(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathSafeWrite)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_execute",
		Arguments: map[string]any{"connection": "rw", "sql": "DROP TABLE users"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if !res.IsError {
		t.Fatal("safe-write must reject DDL")
	}

	assertRowCount(t, ts, 2)
}

// TestMCPSafeWriteEndpointHonoursConnectionPermission checks the connection is
// still the upper bound: "ro" is read-only, so even the safe-write endpoint
// cannot write to it.
func TestMCPSafeWriteEndpointHonoursConnectionPermission(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathSafeWrite)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_execute",
		Arguments: map[string]any{"connection": "ro", "sql": "DELETE FROM users WHERE id = 1"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if !res.IsError {
		t.Fatal("a read-only connection must stay read-only on the safe-write endpoint")
	}

	assertRowCount(t, ts, 2)
}

// TestMCPDisabledEndpointNotMounted verifies a permission level that was not
// enabled is simply absent, rather than mounted and guarded.
func TestMCPDisabledEndpointNotMounted(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Post(ts.URL+pathFull, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a disabled endpoint", resp.StatusCode)
	}
}

// TestMCPQueryToolRejectsWrites checks the query tool refuses writes even on an
// endpoint whose ceiling would have allowed them.
func TestMCPQueryToolRejectsWrites(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathSafeWrite)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_query",
		Arguments: map[string]any{"connection": "rw", "sql": "DELETE FROM users"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if !res.IsError {
		t.Fatal("query tool must reject non-read statements")
	}

	assertRowCount(t, ts, 2)
}

func TestMCPListTables(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_list_tables",
		Arguments: map[string]any{"connection": "ro"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if res.IsError {
		t.Fatalf("tool returned error: %v", contentText(res))
	}

	if text := contentText(res); !strings.Contains(text, "users") {
		t.Errorf("list_tables = %q, want it to contain users", text)
	}
}

// TestMCPSchemaContext checks the compact rendering an agent actually reads.
func TestMCPSchemaContext(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_schema_context",
		Arguments: map[string]any{"connection": "ro"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if res.IsError {
		t.Fatalf("tool returned error: %v", contentText(res))
	}

	text := contentText(res)
	want := []string{
		"users(", "id INTEGER pk", "name TEXT not null", "email TEXT",
		// Foreign keys let the model write correct JOINs instead of guessing.
		"fk: orders.user_id -> users.id",
		// The view marker stops it generating an INSERT against a view.
		"active_users(", "[VIEW, not writable]",
	}

	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("schema context missing %q\n---\n%s", w, text)
		}
	}
}

func assertRowCount(t *testing.T, ts *httptest.Server, want int) {
	t.Helper()

	resp, err := ts.Client().Post(
		ts.URL+"/api/v1/connections/ro/query", "application/json",
		strings.NewReader(`{"sql":"SELECT COUNT(*) FROM users"}`),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var res database.Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(res.Rows) != 1 {
		t.Fatalf("count query returned %d rows", len(res.Rows))
	}

	if got := res.Rows[0][0]; got != float64(want) && got != int64(want) {
		t.Errorf("row count = %v (%T), want %d", got, got, want)
	}
}

func contentText(res *mcp.CallToolResult) string {
	var b strings.Builder

	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}

	return b.String()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return data
}

var _ = context.Background
