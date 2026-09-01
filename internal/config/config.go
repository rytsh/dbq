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
	// Type is the driver name: pgx, odbc, godror, sqlite3 or sqlserver.
	Type string `cfg:"type"`
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
	Host            string        `cfg:"host"`
	Port            string        `cfg:"port" default:"8080"`
	ShutdownTimeout time.Duration `cfg:"shutdown_timeout" default:"10s"`
}

// Address returns the host:port the listener binds to.
func (s Server) Address() string {
	return s.Host + ":" + s.Port
}

// MCP holds the Model Context Protocol server settings.
//
// dbq does not authenticate MCP traffic itself. Instead it mounts one endpoint
// per permission level at a distinct path, so an operator can apply different
// authentication rules to each at the reverse proxy, and can turn the more
// dangerous ones off entirely.
type MCP struct {
	// Enabled turns MCP serving on. When false no endpoint is mounted.
	Enabled bool `cfg:"enabled" default:"true"`
	// Path is the base path the per-permission endpoints hang off.
	Path string `cfg:"path" default:"/mcp"`
	// Endpoints toggles each permission-scoped endpoint.
	Endpoints Endpoints `cfg:"endpoints"`
	// Allow is the default allowlist of connection names exposed over MCP.
	// Empty means every configured connection. An endpoint may override it.
	Allow []string `cfg:"allow"`
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

// Endpoints is the set of permission-scoped MCP endpoints.
//
// Only read-only is on by default: exposing write access to an AI agent is a
// decision the operator has to make deliberately.
type Endpoints struct {
	ReadOnly  Endpoint `cfg:"read_only"`
	SafeWrite Endpoint `cfg:"safe_write"`
	Full      Endpoint `cfg:"full"`
}

// Endpoint is one permission-scoped MCP mount.
type Endpoint struct {
	// Enabled mounts the endpoint.
	Enabled bool `cfg:"enabled"`
	// Path overrides the derived path (<mcp.path>/<permission>).
	Path string `cfg:"path"`
	// Allow overrides mcp.allow for this endpoint only.
	Allow []string `cfg:"allow"`
}

// ResolvedEndpoint is a validated, ready-to-mount MCP endpoint.
type ResolvedEndpoint struct {
	Permission database.Permission
	Path       string
	Allow      []string
}

// ResolvedEndpoints returns the endpoints that should be mounted.
//
// Nothing is mounted when MCP is disabled or every endpoint is off; the caller
// decides whether that is an error.
func (m MCP) ResolvedEndpoints() ([]ResolvedEndpoint, error) {
	if !m.Enabled {
		return nil, nil
	}

	base := m.Path
	if base == "" {
		base = "/mcp"
	}

	base = strings.TrimSuffix(base, "/")

	candidates := []struct {
		permission database.Permission
		endpoint   Endpoint
	}{
		{database.PermissionReadOnly, m.Endpoints.ReadOnly},
		{database.PermissionSafeWrite, m.Endpoints.SafeWrite},
		{database.PermissionFull, m.Endpoints.Full},
	}

	var (
		out   []ResolvedEndpoint
		seen  = map[string]database.Permission{}
		paths []string
	)

	for _, c := range candidates {
		if !c.endpoint.Enabled {
			continue
		}

		path := c.endpoint.Path
		if path == "" {
			path = base + "/" + string(c.permission)
		}

		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		path = strings.TrimSuffix(path, "/")

		// Two permission levels on one path would make the effective
		// permission depend on mount order, which is not something an
		// operator should have to reason about.
		if other, dup := seen[path]; dup {
			return nil, fmt.Errorf(
				"mcp: endpoints %q and %q both use path %s", other, c.permission, path,
			)
		}

		seen[path] = c.permission
		paths = append(paths, path)

		allow := c.endpoint.Allow
		if len(allow) == 0 {
			allow = m.Allow
		}

		out = append(out, ResolvedEndpoint{
			Permission: c.permission,
			Path:       path,
			Allow:      allow,
		})
	}

	// A path that prefixes another would swallow it, because each endpoint is
	// also mounted as a wildcard to catch client-appended segments.
	for _, a := range paths {
		for _, b := range paths {
			if a != b && strings.HasPrefix(b, a+"/") {
				return nil, fmt.Errorf("mcp: path %s is nested under %s", b, a)
			}
		}
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

	// Sane default: MCP on with nothing selected means read-only only. Writing
	// through an AI agent has to be opted into explicitly. To serve no MCP at
	// all, set mcp.enabled to false.
	if cfg.MCP.Enabled && !cfg.MCP.Endpoints.AnyEndpointEnabled() {
		cfg.MCP.Endpoints.ReadOnly.Enabled = true
	}

	slog.Debug("loaded configuration", "config", chu.MarshalMap(cfg))

	return cfg, nil
}

// ConnectionDefs converts the configured profiles into database definitions,
// validating the driver and permission of each.
func (c *Config) ConnectionDefs() ([]database.ConnectionDef, error) {
	defs := make([]database.ConnectionDef, 0, len(c.Connections))

	for name, conn := range c.Connections {
		perm, err := database.ParsePermission(conn.Permission)
		if err != nil {
			return nil, fmt.Errorf("connection %q: %w", name, err)
		}

		defs = append(defs, database.ConnectionDef{
			Name:        name,
			Type:        conn.Type,
			Source:      conn.Source,
			Description: conn.Description,
			Permission:  perm,
		})
	}

	return defs, nil
}

// AnyEndpointEnabled reports whether at least one MCP endpoint is switched on.
func (e Endpoints) AnyEndpointEnabled() bool {
	return e.ReadOnly.Enabled || e.SafeWrite.Enabled || e.Full.Enabled
}
