package main

import (
	"context"
	"os"

	"github.com/rakunlabs/into"
	"github.com/rakunlabs/logi"

	"github.com/rytsh/dbq/internal/cli"
	"github.com/rytsh/dbq/internal/config"
)

// Injected at build time via -ldflags.
var (
	version = "v0.0.0"
	commit  = "-"
	date    = "-"
)

func main() {
	config.Version = version

	into.Init(
		runCommand,
		into.WithLogger(logi.InitializeLog(logi.WithCaller(false))),
		into.WithMsgf("dbq [%s]", version),
		into.WithStartFn(nil),
		into.WithStopFn(nil),
	)
}

func runCommand(ctx context.Context) error {
	root := cli.NewRootCommand(cli.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	})

	root.SetOut(os.Stdout)

	return root.ExecuteContext(ctx)
}
