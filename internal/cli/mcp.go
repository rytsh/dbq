package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/rytsh/dbq/internal/config"
	"github.com/rytsh/dbq/internal/mcpserver"
	"github.com/rytsh/dbq/internal/service"
)

type mcpFlags struct {
	Endpoint string
}

// newMCPCommand runs one configured MCP endpoint over stdin/stdout. Unlike the
// HTTP server, this process serves exactly one client for its lifetime.
func newMCPCommand(global *globalFlags, build BuildInfo) *cobra.Command {
	local := &mcpFlags{}

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "run an MCP server over stdio",
		Long: "Serve one configured MCP endpoint over stdin/stdout without opening a network port.\n\n" +
			"The endpoint supplies the permission ceiling and connection allowlist. " +
			"HTTP-only exports are not available over stdio.",
		Example: "  dbq mcp\n" +
			"  dbq mcp --endpoint /mcp/reporting",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMCP(cmd, global, local, build)
		},
	}

	cmd.Flags().StringVar(&local.Endpoint, "endpoint", "/mcp",
		"configured MCP endpoint whose permissions and connections to use")

	return cmd
}

func runMCP(cmd *cobra.Command, global *globalFlags, local *mcpFlags, build BuildInfo) error {
	ctx := cmd.Context()

	cfg, svc, err := global.load(ctx)
	if err != nil {
		return err
	}
	defer svc.Manager().Close()

	if err := checkConnections(ctx, cfg, svc); err != nil {
		return err
	}

	endpoint, err := resolveStdioEndpoint(cfg.MCP, local.Endpoint)
	if err != nil {
		return err
	}

	scope := service.NewScope(endpoint.Permission, endpoint.Allow, cfg.MCP.MaxRows)
	scope.MaxCellChars = cfg.MCP.MaxCellChars
	scope.Timeout = cfg.MCP.QueryTimeout

	opts := mcpserver.Options{
		Name:            config.ServiceName + "-" + string(endpoint.Permission),
		Version:         build.Version,
		Scope:           scope,
		MaxSchemaTables: cfg.MCP.MaxSchemaTables,
		Logger:          slog.Default(),
	}

	err = mcpserver.New(svc, opts).Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stdio MCP server; %w", err)
	}

	return nil
}

func resolveStdioEndpoint(cfg config.MCP, requested string) (config.ResolvedEndpoint, error) {
	if err := cfg.Validate(); err != nil {
		return config.ResolvedEndpoint{}, err
	}

	endpoints, err := cfg.ResolvedEndpoints()
	if err != nil {
		return config.ResolvedEndpoint{}, err
	}
	if len(endpoints) == 0 {
		return config.ResolvedEndpoint{}, fmt.Errorf("MCP is disabled in configuration")
	}

	path := "/" + strings.Trim(strings.TrimSpace(requested), "/")
	for _, endpoint := range endpoints {
		if endpoint.Path == path {
			return endpoint, nil
		}
	}

	available := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		available = append(available, endpoint.Path)
	}
	slices.Sort(available)

	return config.ResolvedEndpoint{}, fmt.Errorf(
		"MCP endpoint %q is not configured, available: %s", path, strings.Join(available, ", "),
	)
}
