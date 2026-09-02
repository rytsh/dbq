package config

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rakunlabs/chu"
	"github.com/rakunlabs/chu/loader/loaderenv"
	"github.com/rakunlabs/logi"

	"github.com/rytsh/dbq/internal/database"
)

// ServiceName is the config file base name and the env prefix source.
const ServiceName = "dbq"

// EnvPrefix prefixes every environment variable, e.g. DBQ_SERVER_PORT.
const EnvPrefix = "DBQ_"

// Version is overwritten from main so the HTTP config loader can send it.
var Version = "v0.0.0"

// Config is the whole dbq configuration.
//
// It is loaded by chu in the order Default -> File -> HTTP -> Environment, so a
// value set in the environment always wins over the config file.
type Config struct {
	LogLevel string `cfg:"log_level" default:"info"`

	// Connections are the named connection profiles, keyed by name.
	Connections map[string]Connection `cfg:"connections"`

	Server Server `cfg:"server"`
	MCP    MCP    `cfg:"mcp"`
}

// Connection is one named database profile.
type Connection struct {
	// Disabled keeps a profile in configuration without exposing or connecting it.
	Disabled bool `cfg:"disabled"`
	// Type is the driver name: pgx, odbc, godror, sqlite3 or sqlserver.
	Type string `cfg:"type"`
	// Dialect selects database-specific catalog queries when the driver is
	// generic, for example "ingres" for an Ingres ODBC connection.
	Dialect string `cfg:"dialect"`
	// Source is the DSN. Masked in logs because it usually carries a password.
	Source string `cfg:"source" log:"-"`
	// Description is free-form text surfaced to MCP clients to help an agent
	// pick the right database.
	Description string `cfg:"description"`
	// Permission caps what may be run against this connection:
	// read-only, safe-write or full. Defaults to read-only.
	Permission string `cfg:"permission"`
}

// Server holds the HTTP listener settings.
type Server struct {
	Host                   string        `cfg:"host"`
	Port                   string        `cfg:"port" default:"8080"`
	ShutdownTimeout        time.Duration `cfg:"shutdown_timeout" default:"10s"`
	ConnectionCheckTimeout time.Duration `cfg:"connection_check_timeout" default:"10s"`
}

// Address returns the host:port the listener binds to.
func (s Server) Address() string {
	return s.Host + ":" + s.Port
}

// MCP holds the Model Context Protocol server settings.
//
// dbq does not authenticate MCP traffic itself. Operators can mount multiple
// paths with separate upstream authentication, permissions and connections.
type MCP struct {
	// Enabled turns MCP serving on. When false no endpoint is mounted.
	Enabled bool `cfg:"enabled" default:"true"`
	// Endpoints are independently configured MCP paths. Each endpoint applies a
	// permission ceiling and may restrict which connections it exposes.
	Endpoints []Endpoint `cfg:"endpoints"`
	// MaxRows caps rows returned by a single MCP tool call.
	MaxRows int `cfg:"max_rows" default:"200"`
	// MaxSchemaTables caps how many tables dbq_schema_context describes in one call.
	MaxSchemaTables int `cfg:"max_schema_tables" default:"40"`
	// MaxCellChars caps the characters returned for one text value. A row cap
	// alone does not bound a response: a single TEXT column can hold megabytes.
	MaxCellChars int `cfg:"max_cell_chars" default:"500"`
	// QueryTimeout bounds a single MCP statement, so an agent-issued cartesian
	// join cannot pin a connection until the client gives up.
	// It sits below the request timeout most MCP clients use, so a runaway
	// query surfaces as dbq's own actionable error rather than a generic
	// client-side give-up.
	QueryTimeout time.Duration `cfg:"query_timeout" default:"30s"`
	// Stateless runs the MCP handler without session state.
	//
	// It defaults on because it describes what dbq actually is: every tool call
	// resolves a connection, authorizes a statement and returns: nothing is
	// carried between calls. Keeping sessions would only add a failure mode —
	// a request that lands on another replica, or arrives after a restart, is
	// rejected with "session not found" — while buying nothing.
	//
	// Turn it off only for a client that requires the session handshake. Doing
	// so is also a prerequisite for any future server-to-client interaction
	// (elicitation, sampling), which a stateless server rejects outright.
	Stateless bool `cfg:"stateless" default:"true"`
	// JSONResponse returns application/json instead of text/event-stream.
	JSONResponse bool `cfg:"json_response"`
	// SessionTimeout closes idle MCP sessions. Zero means never.
	// Ignored when Stateless is set, because there are no sessions to expire.
	SessionTimeout time.Duration `cfg:"session_timeout" default:"30m"`
}

// Endpoint is one MCP mount with its own permission ceiling and connections.
type Endpoint struct {
	Path       string   `cfg:"path"`
	Permission string   `cfg:"permission"`
	Allow      []string `cfg:"allow"`
}

// ResolvedEndpoint is a validated, ready-to-mount MCP endpoint.
type ResolvedEndpoint struct {
	Permission database.Permission
	Path       string
	Allow      []string
}

// ResolvedEndpoints returns the endpoints that should be mounted.
//
// Nothing is mounted when MCP is disabled. With no explicit endpoints, /mcp is
// mounted with a full ceiling; each connection's permission remains its limit.
func (m MCP) ResolvedEndpoints() ([]ResolvedEndpoint, error) {
	if !m.Enabled {
		return nil, nil
	}

	endpoints := m.Endpoints
	if len(endpoints) == 0 {
		endpoints = []Endpoint{{Path: "/mcp", Permission: string(database.PermissionFull)}}
	}

	var (
		out  []ResolvedEndpoint
		seen = map[string]database.Permission{}
	)

	for _, endpoint := range endpoints {
		path := endpoint.Path
		if path == "" {
			return nil, fmt.Errorf("mcp: endpoint path is required")
		}

		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		path = strings.TrimSuffix(path, "/")

		permission, err := database.ParsePermission(endpoint.Permission)
		if err != nil {
			return nil, fmt.Errorf("mcp: endpoint %s: %w", path, err)
		}

		if other, dup := seen[path]; dup {
			return nil, fmt.Errorf(
				"mcp: endpoints %q and %q both use path %s", other, permission, path,
			)
		}

		seen[path] = permission

		out = append(out, ResolvedEndpoint{
			Permission: permission,
			Path:       path,
			Allow:      endpoint.Allow,
		})
	}

	return out, nil
}

// Load reads the configuration and applies the log level.
func Load(ctx context.Context) (*Config, error) {
	cfg := &Config{}

	if err := chu.Load(ctx, ServiceName, cfg,
		chu.WithLoaderOption(loaderenv.New(
			loaderenv.WithPrefix(EnvPrefix),
		)),
		chu.WithVersion(Version),
	); err != nil {
		return nil, fmt.Errorf("loading config; %w", err)
	}

	if err := logi.SetLogLevel(cfg.LogLevel); err != nil {
		return nil, fmt.Errorf("set log level %s; %w", cfg.LogLevel, err)
	}

	slog.Debug("loaded configuration", "config", chu.MarshalMap(cfg))

	return cfg, nil
}

// ConnectionDefs converts the configured profiles into database definitions,
// validating the driver and permission of each.
func (c *Config) ConnectionDefs() ([]database.ConnectionDef, error) {
	defs := make([]database.ConnectionDef, 0, len(c.Connections))

	for name, conn := range c.Connections {
		if conn.Disabled {
			continue
		}

		perm, err := database.ParsePermission(conn.Permission)
		if err != nil {
			return nil, fmt.Errorf("connection %q: %w", name, err)
		}

		defs = append(defs, database.ConnectionDef{
			Name:        name,
			Type:        conn.Type,
			Dialect:     conn.Dialect,
			Source:      conn.Source,
			Description: conn.Description,
			Permission:  perm,
		})
	}

	return defs, nil
}
