package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/worldline-go/initializer"
	"github.com/worldline-go/logz"

	"github.com/rytsh/query/internal/database"
	"github.com/rytsh/query/internal/input"
)

var (
	version = "v0.0.0"
	commit  = "-"
	date    = "-"
)

var values = struct {
	Source string
	Type   string
	Ping   bool

	InputEcho   bool
	NoDelimeter bool
}{
	Source: "",
	Type:   "",
}

var rootCmd = &cobra.Command{
	Use:           "query",
	Short:         "database query tool",
	Long:          "run query on databases",
	SilenceUsage:  true,
	SilenceErrors: true,
	Example:       "query --source 'postgres://user:urlencodedpassword@localhost:5432/postgres?application_name=query&search_path=myschema' --type pgx",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !values.InputEcho && (values.Source == "" || values.Type == "") {
			return fmt.Errorf("source and type are required")
		}

		return run(cmd.Context())
	},
}

func main() {
	initializer.Init(
		runCommand,
		initializer.WithInitLog(false),
		initializer.WithOptionsLogz(logz.WithCaller(false)),
	)
}

func init() {
	rootCmd.Flags().StringVar(&values.Source, "source", values.Source, "db data source")
	rootCmd.Flags().StringVar(&values.Type, "type", values.Type, "db data source type, supported types: [pgx, odbc, godror, sqlite3, sqlserver]")
	rootCmd.Flags().BoolVar(&values.Ping, "ping", values.Ping, "ping database and exit")
	rootCmd.Flags().BoolVarP(&values.NoDelimeter, "no-delimeter", "n", values.NoDelimeter, "not include delimeter `;`")

	rootCmd.Flags().BoolVar(&values.InputEcho, "echo", values.InputEcho, "echo test input")
}

func runCommand(ctx context.Context, _ *sync.WaitGroup) error {
	rootCmd.Version = version
	rootCmd.Long += "\nversion: " + version + " commit: " + commit + " buildDate:" + date

	return rootCmd.ExecuteContext(ctx)
}

func run(ctx context.Context) error {
	log.Info().Msgf("query [%s] commit: %s buildDate: %s", version, commit, date)

	if values.InputEcho {
		input.Input(ctx, func(ctx context.Context, input string) error {
			fmt.Println(input)

			return nil
		}, input.NoDelimeter(values.NoDelimeter))

		return nil
	}

	db, err := database.ConnectDB(ctx, values.Type, values.Source)
	if err != nil {
		return fmt.Errorf("connecting to database, err: %w", err)
	}
	defer db.Close()

	log.Info().Msgf("connected to database")

	if values.Ping {
		return nil
	}

	// start input loop
	input.Input(ctx, func(ctx context.Context, input string) error {
		rows, err := database.Query(ctx, input, db)
		if err != nil {
			return fmt.Errorf("querying database, err: %w", err)
		}
		defer rows.Close()

		return database.Print(rows)
	}, input.NoDelimeter(values.NoDelimeter))

	return nil
}
