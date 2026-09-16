package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/otterlab-bio/craftmake/internal/cli"
)

var (
	version = "0.1.0-dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	commandContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	command := cli.NewRootCommand(cli.BuildInfo{Version: version, Commit: commit, Date: date})
	if err := command.ExecuteContext(commandContext); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(cli.ExitCode(err))
	}
}
