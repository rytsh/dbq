// Package server serves dbq over HTTP: a small REST API plus the MCP endpoint.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/rakunlabs/ada"
	mcors "github.com/rakunlabs/ada/middleware/cors"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"

	"github.com/rytsh/dbq/internal/config"
	"github.com/rytsh/dbq/internal/database"
	"github.com/rytsh/dbq/internal/mcpserver"
	"github.com/rytsh/dbq/internal/service"
)

// Server wires the ada web server to the dbq service layer.
type Server struct {
	ada     *ada.Server
	svc     *service.Service
	cfg     *config.Config
	address string
}

// New builds the HTTP server, mounting the REST API and, when enabled, the MCP
// streamable HTTP endpoint.
func New(cfg *config.Config, svc *service.Service, version string) (*Server, error) {
	app := ada.New(ada.WithShutdownTimeout(cfg.Server.ShutdownTimeout))

	app.Use(
		mrecover.Middleware(),
		mrequestid.Middleware(),
		mlog.Middleware(),
		mcors.Middleware(),
	)

	s := &Server{
		ada:     app,
		svc:     svc,
		cfg:     cfg,
		address: cfg.Server.Address(),
	}

	app.GET("/healthz", s.health)
	app.GET("/livez", s.live)

	api := app.Group("/api/v1")
	api.GET("/connections", s.listConnections)
	api.GET("/connections/{connection}/tables", s.listTables)
	api.GET("/connections/{connection}/tables/{table}", s.describeTable)
	api.POST("/connections/{connection}/query", s.query)
	api.POST("/query", s.query)

	if err := s.mountMCP(version); err != nil {
		return nil, err
	}

	return s, nil
}

// mountMCP mounts one MCP endpoint per enabled permission level.
//
// Splitting by permission rather than authenticating inside dbq is deliberate:
// each path can be given its own authentication and network policy upstream,
// and the dangerous ones can simply be left unmounted. A connection's own
// permission is still an upper bound, so the /full endpoint cannot write to a
// connection configured as read-only.
func (s *Server) mountMCP(version string) error {
	endpoints, err := s.cfg.MCP.ResolvedEndpoints()
	if err != nil {
		return err
	}

	for _, endpoint := range endpoints {
		scope := service.NewScope(endpoint.Permission, endpoint.Allow, s.cfg.MCP.MaxRows)
		scope.MaxCellChars = s.cfg.MCP.MaxCellChars
		scope.Timeout = s.cfg.MCP.QueryTimeout

		opts := mcpserver.Options{
			Name:            config.ServiceName + "-" + string(endpoint.Permission),
			Version:         version,
			Scope:           scope,
			MaxSchemaTables: s.cfg.MCP.MaxSchemaTables,
			Stateless:       s.cfg.MCP.Stateless,
			JSONResponse:    s.cfg.MCP.JSONResponse,
			Logger:          slog.Default(),
		}

		// A session timeout is meaningless without sessions; passing it anyway
		// would leave a setting that silently does nothing.
		if !s.cfg.MCP.Stateless {
			opts.SessionTimeout = s.cfg.MCP.SessionTimeout
		}

		handler := mcpserver.Handler(mcpserver.New(s.svc, opts), opts)

		// The transport serves POST/GET/DELETE on the mounted path itself, and
		// some clients append a trailing segment, so both forms are registered.
		s.ada.Handle(endpoint.Path, handler)
		s.ada.HandleWildcard(endpoint.Path+"/", handler)

		slog.Info("mcp endpoint mounted",
			"path", endpoint.Path,
			"permission", endpoint.Permission,
			"connections", len(s.svc.Connections(scope)),
			"writable", s.svc.CanWrite(scope),
			"stateless", s.cfg.MCP.Stateless,
		)
	}

	return nil
}

// Handler exposes the routed http.Handler, for tests and for embedding dbq's
// endpoints in another server.
func (s *Server) Handler() http.Handler {
	return s.ada
}

// Start runs the server until ctx is cancelled, then shuts it down gracefully.
//
// ada logs the bound address itself, so nothing is logged here.
func (s *Server) Start(ctx context.Context) error {
	if err := s.ada.StartWithContext(ctx, s.address); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server; %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// apiScope is the scope used by the REST API. It is unrestricted, so each
// connection's own permission is what governs; the MCP ceiling does not apply.
var apiScope = service.FullScope

func (s *Server) live(c *ada.Context) error {
	return c.SendJSON(map[string]string{"status": "ok"})
}

// health pings every connection. A single unreachable database degrades the
// response to 503 but the body still reports each connection individually so
// the failing one is identifiable.
func (s *Server) health(c *ada.Context) error {
	ctx := c.Request.Context()
	statuses := map[string]string{}
	healthy := true

	for _, info := range s.svc.Connections(apiScope) {
		if err := s.svc.Ping(ctx, apiScope, info.Name); err != nil {
			statuses[info.Name] = "error: " + err.Error()
			healthy = false

			continue
		}

		statuses[info.Name] = "ok"
	}

	status := "ok"
	if !healthy {
		status = "degraded"

		c.SetStatus(http.StatusServiceUnavailable)
	}

	return c.SendJSON(map[string]any{"status": status, "connections": statuses})
}

func (s *Server) listConnections(c *ada.Context) error {
	return c.SendJSON(map[string]any{"connections": s.svc.Connections(apiScope)})
}

func (s *Server) listTables(c *ada.Context) error {
	r := c.Request

	tables, err := s.svc.ListTables(
		r.Context(), apiScope, r.PathValue("connection"), r.URL.Query().Get("schema"),
	)
	if err != nil {
		return s.fail(c, err)
	}

	return c.SendJSON(map[string]any{"tables": tables, "count": len(tables)})
}

func (s *Server) describeTable(c *ada.Context) error {
	r := c.Request

	detail, err := s.svc.DescribeTable(
		r.Context(), apiScope,
		r.PathValue("connection"), r.URL.Query().Get("schema"), r.PathValue("table"),
	)
	if err != nil {
		return s.fail(c, err)
	}

	return c.SendJSON(detail)
}

type queryRequest struct {
	// Connection is only read from the body on the unrouted /query endpoint.
	Connection string `json:"connection,omitempty"`
	SQL        string `json:"sql"`
	MaxRows    int    `json:"max_rows,omitempty"`
	// ReadOnly, when true, rejects anything that is not a read regardless of
	// the connection's permission.
	ReadOnly bool `json:"read_only,omitempty"`
}

// maxQueryBodyBytes bounds the request body. A SQL statement is small; anything
// larger is a mistake or an attack.
const maxQueryBodyBytes = 1 << 20

func (s *Server) query(c *ada.Context) error {
	r := c.Request

	// Decoded explicitly rather than through content negotiation: JSON is the
	// only format this endpoint accepts, and binding by Content-Type would
	// silently produce an empty request when the header is missing or wrong.
	var req queryRequest

	decoder := json.NewDecoder(http.MaxBytesReader(c.Response, r.Body, maxQueryBodyBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&req); err != nil {
		return c.SetStatus(http.StatusBadRequest).Err(fmt.Errorf("invalid json body: %w", err))
	}

	connection := r.PathValue("connection")
	if connection == "" {
		connection = req.Connection
	}

	maxRows := req.MaxRows
	if v := r.URL.Query().Get("max_rows"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return c.SetStatus(http.StatusBadRequest).Err(fmt.Errorf("invalid max_rows: %w", err))
		}

		maxRows = parsed
	}

	res, err := s.svc.Execute(r.Context(), apiScope, service.ExecuteRequest{
		Connection: connection,
		SQL:        req.SQL,
		MaxRows:    maxRows,
		ReadOnly:   req.ReadOnly,
	})
	if err != nil {
		return s.fail(c, err)
	}

	return c.SendJSON(res)
}

// fail maps service errors onto HTTP status codes. Permission denials are 403
// and unknown connections are 404; everything else is a 400 because at this
// layer the remaining failures are bad SQL or an unreachable database, both of
// which the caller must act on.
func (s *Server) fail(c *ada.Context, err error) error {
	var stmtErr *database.StatementError

	switch {
	case errors.As(err, &stmtErr):
		return c.SetStatus(http.StatusForbidden).Err(err)
	case errors.Is(err, database.ErrUnknownConnection):
		return c.SetStatus(http.StatusNotFound).Err(err)
	default:
		return c.SetStatus(http.StatusBadRequest).Err(err)
	}
}
