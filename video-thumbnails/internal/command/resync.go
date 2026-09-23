package command

import (
	"context"
	"fmt"
	"os"

	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/resync"
)

// runResync brings the bucket in line with the platform: a job for every
// video without a current master, a list of the masters without a file,
// removed only when asked.
func runResync(ctx context.Context, version string, args []string) error {
	flags := newFlags("resync")
	var spaces repeatable
	flags.Var(&spaces, "space", "Limit the run to this space id; may be given more than once.")
	dryRun := flags.Bool("dry-run", false, "Report what would be done and do nothing.")
	deleteOrphans := flags.Bool("delete-orphans", false, "Remove the masters of files the platform no longer holds. The files inside a folder in the trash bin count as such: the platform lists the folder without their ids, so their masters go too and are rendered again once the folder is restored.")
	if err := flags.Parse(args); err != nil {
		return err
	}

	t, err := openTools(ctx, version, true)
	if err != nil {
		return err
	}
	defer t.Close()

	report, err := resync.Run(ctx, t.gateway, t.thumbs, t.queue, t.video, resync.Options{
		Spaces:        spaces,
		DryRun:        *dryRun,
		DeleteOrphans: *deleteOrphans,
	}, t.log)
	if report != nil {
		printResync(report, *dryRun)
	}
	return err
}

func printResync(report *resync.Report, dryRun bool) {
	raised, removed := "jobs raised", "removed"
	if dryRun {
		raised, removed = "jobs to raise", "to remove"
	}
	fmt.Fprintf(os.Stdout, "spaces %d, videos %d, masters %d, current %d, %s %d (missing %d, stale %d), orphans %d, %s %d\n",
		report.Spaces, report.Videos, report.Masters, report.Current, raised, report.Missing+report.Stale, report.Missing, report.Stale, report.Orphans, removed, report.Deleted)
	if report.Deleted == 0 {
		for _, key := range report.OrphanKeys {
			fmt.Fprintf(os.Stdout, "orphan: %s\n", key)
		}
	}
}
