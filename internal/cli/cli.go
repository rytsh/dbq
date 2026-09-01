// Package cli builds dbq's command tree.
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rytsh/dbq/internal/config"
	"github.com/rytsh/dbq/internal/database"
	"github.com/rytsh/dbq/internal/service"
)

// BuildInfo carries the ldflag-injected build metadata.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// globalFlags are shared by every command.
type globalFlags struct {
	// ConfigFile points chu at a specific config file.
	ConfigFile string
	// Source and Type define an ad-hoc connection from the command line,
	// bypassing the connection profiles in the config file.
	Source string
	Type   string
	// Connection selects a profile from the config file.
	Connection string
	// Permission caps what the ad-hoc connection may run.
	Permission string
}

// adHocName is the profile name given to a connection built from --source/--type.
const adHocName = "cli"

// NewRootCommand builds the dbq command tree.
func NewRootCommand(build BuildInfo) *cobra.Command {
	flags := &globalFlags{}

	root := &cobra.Command{
		Use:           "dbq",
		Short:         "database query tool",
		Long:          "dbq runs SQL against configured databases, interactively or as an HTTP/MCP server",
		Version:       build.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Example: "  dbq --source 'postgres://user:urlencodedpassword@localhost:5432/postgres?application_name=dbq' --type pgx\n" +
			"  dbq --connection prod\n" +
			"  dbq server",
	}

	root.Long += fmt.Sprintf("\nversion: %s commit: %s buildDate: %s", build.Version, build.Commit, build.Date)

	pf := root.PersistentFlags()
	pf.StringVar(&flags.ConfigFile, "config", "", "path to the config file (also settable with CONFIG_FILE)")
	pf.StringVar(&flags.Source, "source", "", "db data source, defines an ad-hoc connection")
	pf.StringVar(&flags.Type, "type", "", fmt.Sprintf("db data source type, supported: %v", database.Drivers))
	pf.StringVarP(&flags.Connection, "connection", "c", "", "named connection from the config file")
	pf.StringVar(&flags.Permission, "permission", string(database.PermissionFull),
		"permission for the ad-hoc connection: read-only, safe-write or full")

	addREPLCommand(root, flags)
	root.AddCommand(newServerCommand(flags, build))
	root.AddCommand(newConnectionsCommand(flags))

	return root
}

// load reads the configuration and builds the service layer.
//
// An ad-hoc --source connection is merged into the loaded config as a named
// profile selected by the REPL.
func (f *globalFlags) load(ctx context.Context) (*config.Config, *service.Service, error) {
	if f.ConfigFile != "" {
		// chu reads CONFIG_FILE; the flag is a friendlier front door for it.
		if err := os.Setenv("CONFIG_FILE", f.ConfigFile); err != nil {
			return nil, nil, fmt.Errorf("setting CONFIG_FILE; %w", err)
		}
	}

	cfg, err := config.Load(ctx)
	if err != nil {
		return nil, nil, err
	}

	if err := f.applyAdHoc(cfg); err != nil {
		return nil, nil, err
	}

	defs, err := cfg.ConnectionDefs()
	if err != nil {
		return nil, nil, err
	}

	if len(defs) == 0 {
		return nil, nil, fmt.Errorf(
			"no connections configured: pass --source and --type, or define connections in %s.yaml",
			config.ServiceName,
		)
	}

	manager, err := database.NewManager(defs)
	if err != nil {
		return nil, nil, err
	}

	return cfg, service.New(manager), nil
}

func (f *globalFlags) applyAdHoc(cfg *config.Config) error {
	if f.Source == "" && f.Type == "" {
		return nil
	}

	if f.Source == "" || f.Type == "" {
		return fmt.Errorf("--source and --type must be given together")
	}

	if _, err := database.ParsePermission(f.Permission); err != nil {
		return err
	}

	if cfg.Connections == nil {
		cfg.Connections = map[string]config.Connection{}
	}

	cfg.Connections[adHocName] = config.Connection{
		Type:        f.Type,
		Source:      f.Source,
		Description: "connection defined on the command line",
		Permission:  f.Permission,
	}
	f.Connection = adHocName

	return nil
}
