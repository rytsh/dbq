package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rytsh/dbq/internal/config"
	"github.com/rytsh/dbq/internal/database"
	"github.com/rytsh/dbq/internal/server"
	"github.com/rytsh/dbq/internal/service"
)

type serverFlags struct {
	Host       string
	Port       string
	MCPEnabled bool
	MCPPath    string
	// MCPEndpoints selects which permission-scoped MCP endpoints to mount,
	// replacing whatever the config file enabled.
	MCPEndpoints []string
}

// newServerCommand runs dbq as an HTTP service exposing probes and MCP.
func newServerCommand(global *globalFlags, build BuildInfo) *cobra.Command {
	local := &serverFlags{}

	cmd := &cobra.Command{
		Use:   "server",
		Short: "run dbq as an HTTP and MCP server",
		Long: "Serve health probes and Model Context Protocol endpoints over HTTP.\n\n" +
			"One MCP endpoint is mounted per permission level, each on its own path\n" +
			"(<mcp.path>/read-only, /safe-write, /full), so each can be given its own\n" +
			"authentication upstream and switched off independently. Only read-only is\n" +
			"mounted unless you say otherwise. A connection's own permission always\n" +
			"remains the upper bound.",
		Example: "  dbq server\n" +
			"  dbq server --mcp-endpoints read-only,safe-write\n" +
			"  dbq server --mcp=false --port 9090",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServer(cmd, global, local, build)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&local.Host, "host", "", "listen host, overrides server.host")
	flags.StringVar(&local.Port, "port", "", "listen port, overrides server.port")
	flags.StringVar(&local.MCPPath, "mcp-path", "", "base path for the MCP endpoints, overrides mcp.path")
	flags.StringSliceVar(&local.MCPEndpoints, "mcp-endpoints", nil,
		"MCP endpoints to mount: read-only, safe-write, full")
	flags.BoolVar(&local.MCPEnabled, "mcp", true, "serve the MCP endpoints")

	return cmd
}

func runServer(cmd *cobra.Command, global *globalFlags, local *serverFlags, build BuildInfo) error {
	ctx := cmd.Context()

	cfg, svc, err := global.load(ctx)
	if err != nil {
		return err
	}
	defer svc.Manager().Close() //nolint:errcheck // shutdown path

	// Flags win over the config file, which in turn already won over defaults.
	if local.Host != "" {
		cfg.Server.Host = local.Host
	}

	if local.Port != "" {
		cfg.Server.Port = local.Port
	}

	if local.MCPPath != "" {
		cfg.MCP.Path = local.MCPPath
	}

	if cmd.Flags().Changed("mcp") {
		cfg.MCP.Enabled = local.MCPEnabled
	}

	if len(local.MCPEndpoints) > 0 {
		if err := applyEndpointFlag(cfg, local.MCPEndpoints); err != nil {
			return err
		}
	}

	srv, err := server.New(cfg, svc, build.Version)
	if err != nil {
		return err
	}

	return srv.Start(ctx)
}

// applyEndpointFlag replaces the configured endpoint selection with the one
// given on the command line, so --mcp-endpoints is an exact list rather than an
// addition to whatever the config file happened to enable.
func applyEndpointFlag(cfg *config.Config, names []string) error {
	cfg.MCP.Endpoints.ReadOnly.Enabled = false
	cfg.MCP.Endpoints.SafeWrite.Enabled = false
	cfg.MCP.Endpoints.Full.Enabled = false

	for _, name := range names {
		permission, err := database.ParsePermission(name)
		if err != nil {
			return fmt.Errorf("--mcp-endpoints: %w", err)
		}

		switch permission {
		case database.PermissionReadOnly:
			cfg.MCP.Endpoints.ReadOnly.Enabled = true
		case database.PermissionSafeWrite:
			cfg.MCP.Endpoints.SafeWrite.Enabled = true
		case database.PermissionFull:
			cfg.MCP.Endpoints.Full.Enabled = true
		}
	}

	return nil
}

// newConnectionsCommand prints the configured connections, which is the
// quickest way to check that a config file was picked up.
func newConnectionsCommand(global *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:           "connections",
		Short:         "list the configured connections",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, svc, err := global.load(cmd.Context())
			if err != nil {
				return err
			}
			defer svc.Manager().Close() //nolint:errcheck // shutdown path

			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")

			if err := enc.Encode(svc.Connections(service.FullScope)); err != nil {
				return fmt.Errorf("encoding connections; %w", err)
			}

			return nil
		},
	}
}
