package command

import (
	"context"
	"fmt"
	"os"

	"github.com/kolsys/opencloud-extensions/file-activity/internal/resync"
)

// runResync brings the tree in line with the platform: every file the
// platform holds is recorded at its path, what it no longer holds is
// forgotten. The blob of a file the service never saw uploaded is read from
// the metadata of the platform when its storage is mounted; without it such
// files are recorded without a blob and listed.
func runResync(ctx context.Context, version string, args []string) error {
	flags := newFlags("resync")
	var spaces repeatable
	flags.Var(&spaces, "space", "Limit the run to this space id; may be given more than once.")
	metadata := flags.String("metadata", "", "Root of the storage of the platform, the directory holding spaces/, mounted read-only: the blobs of files uploaded before the tree are read from it.")
	dryRun := flags.Bool("dry-run", false, "Report what would change and change nothing.")
	if err := flags.Parse(args); err != nil {
		return err
	}

	t, err := openTools(ctx, version, true)
	if err != nil {
		return err
	}
	defer t.Close()

	var blobs resync.BlobIDs
	if *metadata != "" {
		reader, err := resync.NewMetadata(*metadata)
		if err != nil {
			return err
		}
		blobs = reader
	} else {
		t.log.Warn("no metadata given: the files uploaded before the tree keep no blob")
	}

	report, err := resync.Run(ctx, t.gateway, t.tree, blobs, resync.Options{Spaces: spaces, DryRun: *dryRun}, t.log)
	if report != nil {
		printResync(report, *dryRun)
	}
	return err
}

func printResync(report *resync.Report, dryRun bool) {
	verb := "rewritten"
	if dryRun {
		verb = "to rewrite"
	}
	fmt.Fprintf(os.Stdout, "spaces %d, files %d, kept %d, %s %d, stale %d, blobs from metadata %d, without blob %d\n",
		report.Spaces, report.Files, report.Kept, verb, report.Rewritten, report.Stale, report.FromMetadata, len(report.NoBlob))
	for _, file := range report.NoBlob {
		fmt.Fprintf(os.Stdout, "no blob: %s\n", file)
	}
}
