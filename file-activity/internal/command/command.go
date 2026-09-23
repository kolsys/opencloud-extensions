// Package command implements the command line of the service.
package command

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/kolsys/opencloud-extensions/file-activity/internal/config"
)

// Exit codes of the process.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usagePadding is the gap between a command name and its summary.
const usagePadding = 2

type subcommand struct {
	name    string
	summary string
	run     func(ctx context.Context, version string, args []string) error
}

var subcommands = []subcommand{
	{"serve", "Run the service.", runServe},
	{"resync", "Bring the tree in line with the platform.", runResync},
	{"restore", "Write the files of the tree to a WebDAV endpoint from the blobs of the platform.", runRestore},
	{"tap", "Print the raw events of the platform, for taking fixtures off a stand.", runTap},
}

// Execute runs the subcommand named in args and returns the exit code of the
// process. The context of the subcommand is cancelled on SIGINT and SIGTERM.
func Execute(version string, args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitUsage
	}

	switch args[0] {
	case "help", "-h", "--help":
		usage(os.Stdout)
		return exitOK
	case "version", "-v", "--version":
		fmt.Fprintln(os.Stdout, version)
		return exitOK
	}

	for _, cmd := range subcommands {
		if cmd.name != args[0] {
			continue
		}

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if err := cmd.run(ctx, version, args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", config.Name, err)
			return exitError
		}
		return exitOK
	}

	fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", config.Name, args[0])
	usage(os.Stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: %s <command> [flags]\n\nCommands:\n", config.Name)

	table := tabwriter.NewWriter(w, 0, 0, usagePadding, ' ', 0)
	for _, cmd := range subcommands {
		fmt.Fprintf(table, "  %s\t%s\n", cmd.name, cmd.summary)
	}
	fmt.Fprintf(table, "  %s\t%s\n", "version", "Print the version and exit.")
	fmt.Fprintf(table, "  %s\t%s\n", "help", "Print this text and exit.")
	_ = table.Flush()

	fmt.Fprintf(w, "\nThe service is configured through the environment only.\n")
}
