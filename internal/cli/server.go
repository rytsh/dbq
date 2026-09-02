package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/rytsh/dbq/internal/config"
	"github.com/rytsh/dbq/internal/server"
	"github.com/rytsh/dbq/internal/service"
)

type serverFlags struct {
	Host       string
	Port       string
	MCPEnabled bool
}

// newServerCommand runs dbq as an HTTP service exposing probes and MCP.
func newServerCommand(global *globalFlags, build BuildInfo) *cobra.Command {
	local := &serverFlags{}

	cmd := &cobra.Command{
		Use:   "server",
		Short: "run dbq as an HTTP and MCP server",
		Long: "Serve health probes and Model Context Protocol endpoints over HTTP.\n\n" +
			"Each configured MCP endpoint has its own path, permission ceiling and\n" +
			"connection allowlist. A connection's own permission remains the upper bound.",
		Example: "  dbq server\n" +
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
	flags.BoolVar(&local.MCPEnabled, "mcp", true, "serve the MCP endpoints")

	return cmd
}

func runServer(cmd *cobra.Command, global *globalFlags, local *serverFlags, build BuildInfo) error {
	ctx := cmd.Context()

	cfg, svc, err := global.load(ctx)
	if err != nil {
		return err
	}
	defer svc.Manager().Close()

	if err := checkConnections(ctx, cfg, svc); err != nil {
		return err
	}

	// Flags win over the config file, which in turn already won over defaults.
	if local.Host != "" {
		cfg.Server.Host = local.Host
	}

	if local.Port != "" {
		cfg.Server.Port = local.Port
	}

	if cmd.Flags().Changed("mcp") {
		cfg.MCP.Enabled = local.MCPEnabled
	}

	srv, err := server.New(cfg, svc, build.Version)
	if err != nil {
		return err
	}

	return srv.Start(ctx)
}

func checkConnections(ctx context.Context, cfg *config.Config, svc *service.Service) error {
	var errs []error

	statuses := svc.Health(ctx, cfg.Server.ConnectionCheckTimeout)

	for _, name := range slices.Sorted(maps.Keys(statuses)) {
		if err := statuses[name]; err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))

			continue
		}

		slog.Info("database connection check succeeded", "connection", name)
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("database connection checks failed: %w", err)
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
			defer svc.Manager().Close()

			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")

			if err := enc.Encode(svc.Connections(service.FullScope)); err != nil {
				return fmt.Errorf("encoding connections; %w", err)
			}

			return nil
		},
	}
}
