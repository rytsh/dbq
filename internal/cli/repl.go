package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/rytsh/dbq/internal/database"
	"github.com/rytsh/dbq/internal/input"
	"github.com/rytsh/dbq/internal/service"
)

type replFlags struct {
	Ping        bool
	InputEcho   bool
	NoDelimeter bool
	MaxRows     int
}

// addREPLCommand attaches the interactive statement loop to the root command,
// so `dbq` with no subcommand keeps its original behaviour.
func addREPLCommand(root *cobra.Command, global *globalFlags) {
	local := &replFlags{}

	root.RunE = func(cmd *cobra.Command, _ []string) error {
		return runREPL(cmd.Context(), global, local)
	}

	flags := root.Flags()
	flags.BoolVar(&local.Ping, "ping", false, "ping the database and exit")
	flags.BoolVarP(&local.NoDelimeter, "no-delimeter", "n", false, "strip the trailing `;` before sending the statement")
	flags.BoolVar(&local.InputEcho, "echo", false, "echo the input instead of running it, for testing the reader")
	flags.IntVar(&local.MaxRows, "max-rows", database.DefaultMaxRows,
		"maximum rows to fetch per statement, -1 for unlimited")
}

func runREPL(ctx context.Context, global *globalFlags, local *replFlags) error {
	if local.InputEcho {
		return input.Input(ctx, func(_ context.Context, line string) error {
			fmt.Println(line)

			return nil
		}, input.NoDelimeter(local.NoDelimeter))
	}

	_, svc, err := global.load(ctx)
	if err != nil {
		return err
	}
	defer svc.Manager().Close() //nolint:errcheck // shutdown path

	connection := global.Connection
	if connection == "" {
		names := svc.Manager().Names()
		if len(names) != 1 {
			return fmt.Errorf("--connection is required, available: %v", names)
		}

		connection = names[0]
	}

	// The REPL is the operator's own terminal, so the connection's configured
	// permission is the only gate; no extra ceiling is applied.
	scope := service.FullScope
	scope.MaxRows = local.MaxRows

	if err := svc.Ping(ctx, scope, connection); err != nil {
		return fmt.Errorf("connecting to database, err: %w", err)
	}

	if local.Ping {
		slog.Info("connection ok", "connection", connection)

		return nil
	}

	return input.Input(ctx, func(ctx context.Context, line string) error {
		res, err := svc.Execute(ctx, scope, service.ExecuteRequest{
			Connection: connection,
			SQL:        line,
			MaxRows:    local.MaxRows,
		})
		if err != nil {
			return err
		}

		return database.Print(os.Stdout, res)
	}, input.NoDelimeter(local.NoDelimeter))
}
