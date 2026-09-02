package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

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
	Execute     string
	File        string
	Output      string
}

// addREPLCommand attaches the statement runner to the root command, so `dbq`
// with no subcommand keeps its original behaviour: read statements from the
// terminal, a pipe, -e or -f, and print the results.
func addREPLCommand(root *cobra.Command, global *globalFlags) {
	local := &replFlags{}

	root.RunE = func(cmd *cobra.Command, _ []string) error {
		return runREPL(cmd, global, local)
	}

	flags := root.Flags()
	flags.BoolVar(&local.Ping, "ping", false, "ping the database and exit")
	flags.BoolVarP(&local.NoDelimeter, "no-delimeter", "n", false, "strip the trailing `;` before sending the statement")
	flags.BoolVar(&local.InputEcho, "echo", false, "echo the input instead of running it, for testing the reader")
	flags.IntVar(&local.MaxRows, "max-rows", database.DefaultMaxRows,
		"maximum rows to fetch per statement, -1 for unlimited")
	flags.StringVarP(&local.Execute, "execute", "e", "", "run the given SQL and exit; several statements may be separated by `;`")
	flags.StringVarP(&local.File, "file", "f", "", "run the SQL script in the file and exit")
	flags.StringVarP(&local.Output, "output", "o", string(database.FormatTable),
		fmt.Sprintf("output format, one of %v", database.Formats))
}

func runREPL(cmd *cobra.Command, global *globalFlags, local *replFlags) error {
	ctx := cmd.Context()
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	format, err := database.ParseFormat(local.Output)
	if err != nil {
		return err
	}

	if local.Execute != "" && local.File != "" {
		return fmt.Errorf("--execute and --file cannot be used together")
	}

	// Statements come from -e, -f or stdin; the reader is the same either way,
	// and a script never gets an interactive prompt or error recovery.
	source, err := newStatementSource(cmd, local)
	if err != nil {
		return err
	}

	if local.InputEcho {
		return input.Input(ctx, source.reader, func(_ context.Context, line string) error {
			_, err := fmt.Fprintln(stdout, line)

			return err
		}, source.options...)
	}

	_, svc, err := global.load(ctx)
	if err != nil {
		return err
	}
	defer svc.Manager().Close()

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

	run := func(ctx context.Context, sql string) error {
		res, err := svc.Execute(ctx, scope, service.ExecuteRequest{
			Connection: connection,
			SQL:        sql,
			MaxRows:    local.MaxRows,
		})
		if err != nil {
			return err
		}

		return writeResult(stdout, stderr, res, format)
	}

	return input.Input(ctx, source.reader, run, source.options...)
}

type statementSource struct {
	reader  io.Reader
	options []input.Option
}

// newStatementSource picks where statements come from: -e, -f, or stdin.
func newStatementSource(cmd *cobra.Command, local *replFlags) (statementSource, error) {
	options := []input.Option{input.NoDelimeter(local.NoDelimeter)}

	switch {
	case local.Execute != "":
		return statementSource{
			reader:  strings.NewReader(local.Execute),
			options: append(options, input.Interactive(false)),
		}, nil
	case local.File != "":
		content, err := os.ReadFile(local.File)
		if err != nil {
			return statementSource{}, fmt.Errorf("reading %s; %w", local.File, err)
		}

		return statementSource{
			reader:  strings.NewReader(string(content)),
			options: append(options, input.Interactive(false)),
		}, nil
	default:
		return statementSource{reader: cmd.InOrStdin(), options: options}, nil
	}
}

// writeResult renders the result on stdout. Machine-readable formats stay
// clean, so their truncation notices go to stderr.
func writeResult(stdout, stderr io.Writer, res *database.Result, format database.Format) error {
	if err := database.Write(stdout, res, format); err != nil {
		return err
	}

	if format == database.FormatTable {
		return nil
	}

	if res.Truncated {
		fmt.Fprintf(stderr, "warning: result truncated at %d rows\n", res.RowCount)
	}

	if res.CellsTruncated > 0 {
		fmt.Fprintf(stderr, "warning: %d value(s) shortened\n", res.CellsTruncated)
	}

	return nil
}
