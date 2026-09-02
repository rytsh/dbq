// Package server serves health probes and MCP over HTTP.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rakunlabs/ada"
	mcors "github.com/rakunlabs/ada/middleware/cors"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"

	"github.com/rytsh/dbq/internal/config"
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

// New builds the HTTP server, mounting health probes and, when enabled, the MCP
// streamable HTTP endpoints.
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

	if err := s.mountMCP(version); err != nil {
		return nil, err
	}

	return s, nil
}

// mountMCP mounts every configured MCP endpoint.
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

		// Mount endpoints exactly so /mcp and /mcp/abc can coexist.
		s.ada.Handle(endpoint.Path, handler)
		s.ada.Handle(endpoint.Path+"/", handler)

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

func (s *Server) live(c *ada.Context) error {
	return c.SendJSON(map[string]string{"status": "ok"})
}

// health pings every connection. A single unreachable database degrades the
// response to 503 but the body still reports each connection individually so
// the failing one is identifiable. Each ping is bounded by the connection
// check timeout so a hung database cannot hang the probe.
func (s *Server) health(c *ada.Context) error {
	statuses := map[string]string{}
	healthy := true

	for name, err := range s.svc.Health(c.Request.Context(), s.cfg.Server.ConnectionCheckTimeout) {
		if err != nil {
			statuses[name] = "error: " + err.Error()
			healthy = false

			continue
		}

		statuses[name] = "ok"
	}

	status := "ok"
	if !healthy {
		status = "degraded"

		c.SetStatus(http.StatusServiceUnavailable)
	}

	return c.SendJSON(map[string]any{"status": status, "connections": statuses})
}
