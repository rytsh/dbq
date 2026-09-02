package server_test

import (
	"encoding/json"
	"io"
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
	return newTestServerWithExport(t, true)
}

func newTestServerWithExport(t *testing.T, exportEnabled bool) *httptest.Server {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "test.db")

	cfg := &config.Config{
		Connections: map[string]config.Connection{
			"ro": {Type: "sqlite3", Source: dsn, Permission: "read-only", Description: "read only"},
			"rw": {Type: "sqlite3", Source: dsn, Permission: "full"},
		},
		Server: config.Server{Port: "0", ShutdownTimeout: time.Second},
		MCP: config.MCP{
			Enabled:        true,
			ExportEnabled:  exportEnabled,
			MaxRows:        10,
			Stateless:      true,
			AllowedOrigins: []string{"https://client.example"},
			Endpoints: []config.Endpoint{
				{Path: "/mcp", Permission: "read-only", Export: exportEnabled},
				{Path: "/mcp/write", Permission: "safe-write"},
			},
		},
	}

	defs, err := cfg.ConnectionDefs()
	if err != nil {
		t.Fatalf("connection defs: %v", err)
	}

	manager, err := database.NewManager(defs)
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
	t.Cleanup(func() { _ = srv.Close() })

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

	if len(body.Connections) != 2 {
		t.Errorf("connections = %v, want 2 entries", body.Connections)
	}
}

func TestLivez(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/livez")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRESTEndpointsNotMounted(t *testing.T) {
	ts := newTestServer(t)

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/connections"},
		{http.MethodGet, "/api/v1/connections/ro/tables"},
		{http.MethodGet, "/api/v1/connections/ro/tables/users"},
		{http.MethodPost, "/api/v1/connections/ro/query"},
		{http.MethodPost, "/api/v1/query"},
	}

	for _, tt := range tests {
		req, err := http.NewRequest(tt.method, ts.URL+tt.path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("request %s %s: %v", tt.method, tt.path, err)
		}

		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("do %s %s: %v", tt.method, tt.path, err)
		}

		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", tt.method, tt.path, resp.StatusCode)
		}
	}
}

// connectMCP performs a real MCP handshake against one of the permission-scoped
// endpoints, e.g. path "/mcp".
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
	pathReadOnly  = "/mcp"
	pathSafeWrite = "/mcp/write"
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
		"dbq_schema_context", "dbq_query", "dbq_export",
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

func TestMCPExportIsOptIn(t *testing.T) {
	ts := newTestServerWithExport(t, false)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "dbq_export" {
			t.Fatal("dbq_export advertised without export_enabled")
		}
	}

	resp, err := ts.Client().Get(ts.URL + "/exports/not-found/file.sql")
	if err != nil {
		t.Fatalf("get export route: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("export route status = %d, want 404", resp.StatusCode)
	}
}

func TestMCPExportInputSchemaHasConstraints(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "dbq_export" {
			continue
		}

		schema := mustJSON(t, tool.InputSchema)
		if !strings.Contains(string(schema), `"maximum":1000`) ||
			!strings.Contains(string(schema), `"enum":["postgresql","sqlite3","sqlserver","oracle","mysql","ingres","odbc"]`) {
			t.Fatalf("export schema lacks constraints: %s", schema)
		}

		return
	}

	t.Fatal("dbq_export not advertised")
}

func TestMCPRejectsCrossOriginRequests(t *testing.T) {
	ts := newTestServer(t)
	req, err := http.NewRequest(http.MethodPost, ts.URL+pathReadOnly, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestMCPAllowsConfiguredOriginPreflight(t *testing.T) {
	ts := newTestServer(t)
	req, err := http.NewRequest(http.MethodOptions, ts.URL+pathReadOnly, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type,mcp-protocol-version")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://client.example" {
		t.Errorf("allow origin = %q", got)
	}
	if got := strings.ToLower(resp.Header.Get("Access-Control-Allow-Headers")); !strings.Contains(got, "content-type") {
		t.Errorf("allow headers = %q", got)
	}
}

func TestMCPExportReturnsManifestAndDownload(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "dbq_export",
		Arguments: map[string]any{
			"connection": "ro", "sql": "SELECT id, name FROM users ORDER BY id",
			"target_table": "imported_users", "batch_size": 2,
		},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", contentText(res))
	}

	var out struct {
		DownloadURL string `json:"download_url"`
		RowCount    int    `json:"row_count"`
	}
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RowCount != 2 || !strings.HasPrefix(out.DownloadURL, "/exports/") {
		t.Fatalf("manifest = %+v", out)
	}
	if text := contentText(res); strings.Contains(text, "Ada") || strings.Contains(text, "Grace") {
		t.Fatalf("MCP result leaked exported values: %s", text)
	}

	resp, err := ts.Client().Get(ts.URL + out.DownloadURL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	if got := string(data); !strings.Contains(got, "INSERT INTO \"imported_users\"") || !strings.Contains(got, "'ada'") {
		t.Errorf("download = %q", got)
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

func TestMCPQueryRequiresConnection(t *testing.T) {
	ts := newTestServer(t)
	session := connectMCP(t, ts, pathReadOnly)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_query",
		Arguments: map[string]any{"sql": "SELECT 1"},
	})
	if err == nil && !res.IsError {
		t.Fatal("dbq_query must reject a request without a connection")
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

// TestMCPUnconfiguredEndpointNotMounted verifies an absent path returns 404.
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

	structured := mustJSON(t, res.StructuredContent)
	if strings.Contains(string(structured), `"tables"`) {
		t.Errorf("schema structured content duplicates full table details: %s", structured)
	}
}

func assertRowCount(t *testing.T, ts *httptest.Server, want int) {
	t.Helper()

	session := connectMCP(t, ts, pathReadOnly)
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "dbq_query",
		Arguments: map[string]any{"connection": "ro", "sql": "SELECT COUNT(*) FROM users"},
	})
	if err != nil {
		t.Fatalf("count query: %v", err)
	}

	var out struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(out.Rows) != 1 {
		t.Fatalf("count query returned %d rows", len(out.Rows))
	}

	if got := out.Rows[0][0]; got != float64(want) && got != int64(want) {
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
